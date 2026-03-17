package filterremapprocessor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

type traceData struct {
	Ctx                  context.Context
	ArrivalTime          time.Time
	LastSpanArrivalNanos atomic.Int64
	HierarchyMap         hierarchyMap
	SpanCount            atomic.Int64
}

type hierarchyMap struct {
	m map[pcommon.SpanID]hierarchyNode
	sync.RWMutex
}

func (hm *hierarchyMap) get(spanId pcommon.SpanID) (hierarchyNode, bool) {
	hm.RLock()
	defer hm.RUnlock()
	n, ok := hm.m[spanId]
	return n, ok
}

func (hm *hierarchyMap) set(spanId pcommon.SpanID, node hierarchyNode) {
	hm.Lock()
	defer hm.Unlock()
	hm.m[spanId] = node
}

func (hm *hierarchyMap) remapHierarchy(remapOrphanedSpans bool) {
	hm.Lock()
	defer hm.Unlock()
	for spanId, node := range hm.m {
		if node.IsParentRetained() {
			continue
		}

		switch node.(type) {
		case *retainedHierarchyNode:
			hm.TrySetNextRetainedParent(spanId, remapOrphanedSpans)
		default:
			continue
		}
	}
}

// TrySetNextRetainedParent takes a spanId and iterates through the parent hierarchy from this span until it finds a retained parent
// it sets this parent as the parent for any dropped spans that we see along the way, to avoid having to do lookups for all of the dropped nodes for every span
// If remapOrphanedSpans is false, orphaned spans will keep their original parent ID; if true, they will be remapped to root
// Orphaned spans may occur if a batch of spans arrive after the last span timeout when the trace has already been forwarded.
func (hm *hierarchyMap) TrySetNextRetainedParent(spanId pcommon.SpanID, remapOrphanedSpans bool) {
	node, ok := hm.m[spanId]
	if !ok {
		return
	}

	parentRetained := node.IsParentRetained()
	if parentRetained {
		return
	}

	parentId := node.ParentID()

	intermediateDroppedNodes := make([]*droppedHierarchyNode, 0)
	visitedNodes := make(map[pcommon.SpanID]bool, 64)
	visitedNodes[spanId] = true

	for !parentRetained {
		if parentId.IsEmpty() {
			//need to do this just to set parent retained if the parent is null.
			//this will only happen if dropRootSpans is true and the root span is dropped
			for _, dNode := range intermediateDroppedNodes {
				dNode.setParentId(parentId, true)
			}
			node.setParentId(parentId, true)
			return
		}

		if _, ok := visitedNodes[parentId]; ok {
			//we've already visited this node, so we'll break out of the loop as this indicates a cycle
			break
		}
		visitedNodes[parentId] = true

		parentNode, ok := hm.m[parentId]
		if !ok {
			//this is the highest parent we can set, we'll set it as the parent and return
			// If remapOrphanedSpans is false, keep the original parentId; if true, remap to root (empty SpanID)
			newParentId := parentId
			if remapOrphanedSpans {
				newParentId = pcommon.SpanID{}
			}
			for _, dNode := range intermediateDroppedNodes {
				dNode.setParentId(newParentId, false)
			}
			node.setParentId(newParentId, false)
			return
		}

		switch parentNode.(type) {
		case *retainedHierarchyNode:
			node.setParentId(parentId, true)
			for _, dNode := range intermediateDroppedNodes {
				//set this as the retained parent for all dropped nodes, on next lookups this will make things much faster
				dNode.setParentId(parentId, true)
			}
			return
		case *droppedHierarchyNode:
			intermediateDroppedNodes = append(intermediateDroppedNodes, parentNode.(*droppedHierarchyNode))
			parentId = parentNode.ParentID()

		}
		parentRetained = parentNode.IsParentRetained()
		if parentRetained {
			parentId = parentNode.ParentID()
			for _, dNode := range intermediateDroppedNodes {
				dNode.setParentId(parentId, true)
			}
			node.setParentId(parentId, true)
			return
		}
	}
}
