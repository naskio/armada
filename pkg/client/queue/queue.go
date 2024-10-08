package queue

import (
	"fmt"

	armadamaps "github.com/armadaproject/armada/internal/common/maps"
	"github.com/armadaproject/armada/pkg/api"
)

type Queue struct {
	Name                              string         `json:"name"`
	Permissions                       []Permissions  `json:"permissions"`
	PriorityFactor                    PriorityFactor `json:"priorityFactor"`
	ResourceLimitsByPriorityClassName map[string]api.PriorityClassResourceLimits
	Cordoned                          bool              `json:"cordoned"`
	Labels                            map[string]string `json:"labels"`
}

// NewQueue returns new Queue using the in parameter. Error is returned if
// any of the queue fields has corresponding value that is invalid.
func NewQueue(in *api.Queue) (Queue, error) {
	if in == nil {
		return Queue{}, fmt.Errorf("queue is nil")
	}

	priorityFactor, err := NewPriorityFactor(in.PriorityFactor)
	if err != nil {
		return Queue{}, fmt.Errorf("failed to map priority factor. %s", err)
	}

	permissions := []Permissions{}
	if len(in.GroupOwners) != 0 || len(in.UserOwners) != 0 {
		permissions = append(permissions, NewPermissionsFromOwners(in.UserOwners, in.GroupOwners))
	}

	for index, permission := range in.Permissions {
		perm, err := NewPermissions(permission)
		if err != nil {
			return Queue{}, fmt.Errorf("failed to map permission with index: %d. %s", index, err)
		}
		permissions = append(permissions, perm)
	}

	resourceLimitsByPriorityClassName := make(map[string]api.PriorityClassResourceLimits, len(in.ResourceLimitsByPriorityClassName))
	for k, v := range in.ResourceLimitsByPriorityClassName {
		if v != nil {
			resourceLimitsByPriorityClassName[k] = *v
		}
	}

	// Queue labels must be Kubernetes-like key-value labels
	for k, v := range in.Labels {
		if k == "" || v == "" {
			return Queue{}, fmt.Errorf("queue labels must not have an empty key or value, key: %s, value: %s", k, v)
		}
	}

	return Queue{
		Name:                              in.Name,
		PriorityFactor:                    priorityFactor,
		Permissions:                       permissions,
		ResourceLimitsByPriorityClassName: resourceLimitsByPriorityClassName,
		Cordoned:                          in.Cordoned,
		Labels:                            in.Labels,
	}, nil
}

// ToAPI transforms Queue to *api.Queue structure
func (q Queue) ToAPI() *api.Queue {
	rv := &api.Queue{
		Name:           q.Name,
		PriorityFactor: float64(q.PriorityFactor),
		ResourceLimitsByPriorityClassName: armadamaps.MapValues(
			q.ResourceLimitsByPriorityClassName,
			func(p api.PriorityClassResourceLimits) *api.PriorityClassResourceLimits {
				return &p
			}),
		Cordoned: q.Cordoned,
		Labels:   q.Labels,
	}
	for _, permission := range q.Permissions {
		rv.Permissions = append(rv.Permissions, permission.ToAPI())
	}
	return rv
}

// HasPermission returns true if the inputSubject is allowed to peform a queue operation
// specified by inputVerb parameter, otherwise returns false
func (q Queue) HasPermission(inputSubject PermissionSubject, inputVerb PermissionVerb) bool {
	for _, permission := range q.Permissions {
		for _, subject := range permission.Subjects {
			if subject == inputSubject {
				for _, verb := range permission.Verbs {
					if verb == inputVerb {
						return true
					}
				}
			}
		}
	}

	return false
}

func QueuesToAPI(queues []Queue) []*api.Queue {
	result := make([]*api.Queue, len(queues))

	for index, queue := range queues {
		result[index] = queue.ToAPI()
	}

	return result
}
