// SPDX-License-Identifier: AGPL-3.0-only

package querymiddleware

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/grafana/dskit/cache"
	"github.com/grafana/dskit/tenant"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"

	apierror "github.com/grafana/mimir/pkg/api/error"
	"github.com/grafana/mimir/pkg/util/spanlogger"
	"github.com/grafana/mimir/pkg/util/validation"
)

const (
	reasonDisabledByOption  = "disable-by-option"
	reasonNotAPIError       = "not-api-error"
	reasonNotCacheableError = "not-cacheable-api-error"
)

func newErrorCachingMiddleware(cache cache.Cache, limits Limits, ttl time.Duration, shouldCacheReq shouldCacheFn, logger log.Logger, reg prometheus.Registerer) MetricsQueryMiddleware {
	cacheAttempted := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "cortex_frontend_query_error_cache_requests_total",
		Help: "",
	})
	cacheHits := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "cortex_frontend_query_error_cache_hits_total",
		Help: "",
	})
	cacheStoreSkipped := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "cortex_frontend_query_error_cache_skipped_total",
		Help: "",
	}, []string{"reason"})

	return MetricsQueryMiddlewareFunc(func(next MetricsQueryHandler) MetricsQueryHandler {
		return &errorCachingHandler{
			next:              next,
			cache:             cache,
			limits:            limits,
			ttl:               ttl,
			shouldCacheReq:    shouldCacheReq,
			logger:            logger,
			cacheAttempted:    cacheAttempted,
			cacheHits:         cacheHits,
			cacheStoreSkipped: cacheStoreSkipped,
		}
	})
}

type errorCachingHandler struct {
	next           MetricsQueryHandler
	cache          cache.Cache
	limits         Limits
	ttl            time.Duration
	shouldCacheReq shouldCacheFn
	logger         log.Logger

	cacheAttempted    prometheus.Counter
	cacheHits         prometheus.Counter
	cacheStoreSkipped *prometheus.CounterVec

	// TODO: Cache store attempted metric? skipped vs stored?
}

func (e *errorCachingHandler) Do(ctx context.Context, request MetricsQueryRequest) (Response, error) {
	spanLog := spanlogger.FromContext(ctx, e.logger)
	tenantIDs, err := tenant.TenantIDs(ctx)
	if err != nil {
		return e.next.Do(ctx, request)
	}

	// Check if caching has disabled via an option on the request
	if !e.shouldCacheReq(request) {
		e.cacheStoreSkipped.WithLabelValues(reasonDisabledByOption).Inc()
		return e.next.Do(ctx, request)
	}

	e.cacheAttempted.Inc()
	key := e.cacheKey(tenant.JoinTenantIDs(tenantIDs), request)
	hashedKey := cacheHashKey(key)

	if cachedErr := e.loadErrorFromCache(ctx, key, hashedKey); cachedErr != nil {
		e.cacheHits.Inc()
		spanLog.DebugLog(
			"msg", "returned cached API error",
			"error_type", cachedErr.Type,
			"key", key,
			"hashed_key", hashedKey,
		)

		return nil, cachedErr
	}

	res, err := e.next.Do(ctx, request)
	if err != nil {
		var apiErr *apierror.APIError
		if !errors.As(err, &apiErr) {
			e.cacheStoreSkipped.WithLabelValues(reasonNotAPIError).Inc()
			return res, err
		}

		if cacheable, reason := e.isCacheable(apiErr, request, tenantIDs); !cacheable {
			e.cacheStoreSkipped.WithLabelValues(reason).Inc()
			spanLog.DebugLog(
				"msg", "error result from request is not cacheable",
				"error_type", apiErr.Type,
				"reason", reason,
			)
			return res, err
		}

		e.storeErrorToCache(key, hashedKey, apiErr)
	}

	return res, err
}

func (e *errorCachingHandler) loadErrorFromCache(ctx context.Context, key, hashedKey string) *apierror.APIError {
	res := e.cache.GetMulti(ctx, []string{hashedKey})
	if cached, ok := res[hashedKey]; ok {
		var cachedError CachedError
		if err := proto.Unmarshal(cached, &cachedError); err != nil {
			level.Warn(e.logger).Log("msg", "unable to unmarshall cached error", "err", err)
			return nil
		}

		if cachedError.GetKey() != key {
			level.Debug(e.logger).Log(
				"msg", "cached error key does not match",
				"expected_key", key,
				"actual_key", cachedError.GetKey(),
				"hashed_key", hashedKey,
			)
			return nil
		}

		return apierror.New(apierror.Type(cachedError.Type), cachedError.Message)
	}

	return nil
}

func (e *errorCachingHandler) storeErrorToCache(key, hashedKey string, apiErr *apierror.APIError) {
	bytes, err := proto.Marshal(&CachedError{
		Key:     key,
		Type:    string(apiErr.Type),
		Message: apiErr.Message,
	})

	if err != nil {
		level.Warn(e.logger).Log("msg", "unable to marshal cached error", "err", err)
		return
	}

	e.cache.SetAsync(hashedKey, bytes, e.ttl)
}

func (e *errorCachingHandler) cacheKey(tenantID string, r MetricsQueryRequest) string {
	return fmt.Sprintf("EC:%s:%s:%d:%d:%d", tenantID, r.GetQuery(), r.GetStart(), r.GetEnd(), r.GetStep())
}

func (e *errorCachingHandler) isCacheable(apiErr *apierror.APIError, req MetricsQueryRequest, tenantIDs []string) (bool, string) {
	if apiErr.Type != apierror.TypeBadData && apiErr.Type != apierror.TypeExec {
		return false, reasonNotCacheableError
	}

	maxCacheFreshness := validation.MaxDurationPerTenant(tenantIDs, e.limits.MaxCacheFreshness)
	maxCacheTime := int64(model.Now().Add(-maxCacheFreshness))
	cacheUnalignedRequests := validation.AllTrueBooleansPerTenant(tenantIDs, e.limits.ResultsCacheForUnalignedQueryEnabled)

	return isRequestCachable(req, maxCacheTime, cacheUnalignedRequests, e.logger)
}
