package server

import (
	"context"

	"k8s.io/klog/v2"
)

type DestHostBackendManager struct {
	*DefaultBackendStorage
}

var _ BackendManager = &DestHostBackendManager{}

func NewDestHostBackendManager() *DestHostBackendManager {
	return &DestHostBackendManager{DefaultBackendStorage: NewDefaultBackendStorage()}
}

// Backend tries to get a backend associating to the request destination host.
func (dibm *DestHostBackendManager) Backend(ctx context.Context) (Backend, error) {
	dibm.mu.RLock()
	defer dibm.mu.RUnlock()
	if len(dibm.backends) == 0 {
		return nil, &ErrNotFound{}
	}
	destHost := ctx.Value("destHost").(string)
	if destHost != "" {
		bes, exist := dibm.backends[destHost]
		if exist && len(bes) > 0 {
			klog.V(5).InfoS("Get the backend through the DestHostBackendManager", "destHost", destHost)
			return dibm.backends[destHost][0], nil
		}
	}
	return nil, &ErrNotFound{}
}
