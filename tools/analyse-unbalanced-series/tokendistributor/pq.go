package tokendistributor

import (
	"container/heap"
	"fmt"
	"math"
)

type CandidateTokenInfoOwnership struct {
	candidateTokenInfo *candidateTokenInfo
	ownership          float64
	index              int
}

func newCandidateTokenInfoOwnership(cand *candidateTokenInfo) *CandidateTokenInfoOwnership {
	return &CandidateTokenInfoOwnership{
		candidateTokenInfo: cand,
		ownership:          cand.getReplicatedOwnership(),
	}
}

type PriorityQueue struct {
	items []*CandidateTokenInfoOwnership
	max   bool
}

func newPriorityQueue(len int, max bool) *PriorityQueue {
	pq := &PriorityQueue{
		items: make([]*CandidateTokenInfoOwnership, 0, len),
		max:   max,
	}

	return pq
}

func (pq PriorityQueue) Len() int {
	return len(pq.items)
}

func (pq PriorityQueue) Swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
	pq.items[i].index = i // pq[i] is actually the old pq[j], so pq[i].index should be updated to i
	pq.items[j].index = j // pq[j] is actually the old pq[i], so pq[j].index should be updated to j
}

func (pq PriorityQueue) Less(i, j int) bool {
	if pq.max {
		// we are implementing a max heap priority queue, so we are using > here
		// Since we compare float64, NaN values must be placed at the end
		return pq.items[i].ownership > pq.items[j].ownership || (math.IsNaN(pq.items[j].ownership) && !math.IsNaN(pq.items[i].ownership))
	}
	return pq.items[i].ownership < pq.items[j].ownership || (math.IsNaN(pq.items[i].ownership) && !math.IsNaN(pq.items[j].ownership))
}

// Push implements heap.Push(any). It pushes the element item onto PriorityQueue.
// The complexity is O(log n) where n = PriorityQueue.Len().
func (pq *PriorityQueue) Push(item any) {
	n := len(pq.items)
	candidateTokenInfoOwnership := item.(*CandidateTokenInfoOwnership)
	candidateTokenInfoOwnership.index = n
	pq.items = append(pq.items, candidateTokenInfoOwnership)
}

// Pop implements heap.Pop(). It removes and returns the element with the highest priority from PriorityQueue.
// The complexity is O(log n) where n = PriorityQueue.Len().
func (pq *PriorityQueue) Pop() any {
	old := pq.items
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	pq.items = old[0 : n-1]
	return item
}

// Peek the returns the element with the highest priority from PriorityQueue, but it does not remove it from the latter.
// The complexity is O(1).
func (pq *PriorityQueue) Peek() *CandidateTokenInfoOwnership {
	return (pq.items)[0]
}

// Update updates the element candidateTokenInfoOwnership passed as parameter by applying to it  the updating function
// update passed as parameter, and propagate this modification to PriorityQueue. Element candidateTokenInfoOwnership
// must be already present on PriorityQueue.
func (pq *PriorityQueue) Update(candidateTokenInfoOwnership *CandidateTokenInfoOwnership, update func(*CandidateTokenInfoOwnership)) {
	update(candidateTokenInfoOwnership)
	heap.Fix(pq, candidateTokenInfoOwnership.index)
}

// Add adds an element at the end of the queue, but it does not take into account the ownership value.
// In order to re-stabilize the priority queue property it is necessary to call heap.Init() on this queue.
func (pq *PriorityQueue) Add(candidateTokenInfoOwnership *CandidateTokenInfoOwnership) {
	pq.items = append(pq.items, candidateTokenInfoOwnership)
}

func (pq *PriorityQueue) String() string {
	if pq.Len() == 0 {
		return "[]"
	}

	str := "["
	for i, item := range pq.items {
		str += fmt.Sprintf("%d: %d-%.2f", i, item.candidateTokenInfo.token, item.ownership)
		if i < pq.Len()-1 {
			str += ","
		}
	}
	str += "]"
	return str
}
