package extender

import "encoding/json"

// The kube-scheduler extender wire types, hand-rolled from
// k8s.io/kube-scheduler/extender/v1 so the module keeps zero Kubernetes
// dependencies. Only the fields the extender reads are decoded; Nodes is
// kept raw so a filter pass-through returns the node objects unchanged.

// Args is the body of a filter or prioritize call (ExtenderArgs).
type Args struct {
	Pod       Pod             `json:"pod"`
	Nodes     json.RawMessage `json:"nodes,omitempty"`
	NodeNames *[]string       `json:"nodenames,omitempty"`
}

// FilterResult is the body of a filter reply (ExtenderFilterResult).
type FilterResult struct {
	Nodes                      json.RawMessage   `json:"nodes,omitempty"`
	NodeNames                  *[]string         `json:"nodenames,omitempty"`
	FailedNodes                map[string]string `json:"failedNodes,omitempty"`
	FailedAndUnresolvableNodes map[string]string `json:"failedAndUnresolvableNodes,omitempty"`
	Error                      string            `json:"error,omitempty"`
}

// HostPriority is one entry of a prioritize reply (HostPriorityList).
type HostPriority struct {
	Host  string `json:"host"`
	Score int64  `json:"score"`
}

// Pod holds the pod fields the extender reads.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
}

// ObjectMeta holds the object metadata fields the extender reads.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	UID         string            `json:"uid"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// NodeList holds the node fields the extender reads (v1.NodeList).
type NodeList struct {
	Items []Node `json:"items"`
}

// Node holds the node fields the extender reads (v1.Node).
type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Addresses []NodeAddress `json:"addresses"`
	} `json:"status"`
}

// NodeAddress is one entry of v1.NodeStatus.Addresses.
type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// InternalIP returns the node's first InternalIP address, or "".
func (n Node) InternalIP() string {
	for _, a := range n.Status.Addresses {
		if a.Type == "InternalIP" {
			return a.Address
		}
	}
	return ""
}
