// Package yandexapi — узкая прослойка над Compute API Yandex Cloud: только то,
// что нужно плагину, и плоские структуры вместо protobuf.
package yandexapi

import (
	"context"
	"errors"
)

var (
	// ErrNotFound — инстанса/образа нет.
	ErrNotFound = errors.New("not found")
	// ErrResourceExhausted — не хватило квоты или ресурсов зоны/платформы.
	ErrResourceExhausted = errors.New("resource exhausted")
)

// Статусы инстанса, как их отдаёт Compute API.
const (
	StatusProvisioning = "PROVISIONING"
	StatusRunning      = "RUNNING"
	StatusStopping     = "STOPPING"
	StatusStopped      = "STOPPED"
	StatusStarting     = "STARTING"
	StatusRestarting   = "RESTARTING"
	StatusUpdating     = "UPDATING"
	StatusError        = "ERROR"
	StatusCrashed      = "CRASHED"
	StatusDeleting     = "DELETING"
)

type Instance struct {
	ID     string
	Name   string
	Status string
	Labels map[string]string

	// Адреса первого сетевого интерфейса.
	InternalIP string
	ExternalIP string
}

type CreateInstanceRequest struct {
	FolderID string
	Name     string
	Labels   map[string]string
	Metadata map[string]string

	Zone       string
	SubnetID   string
	PlatformID string

	Cores        int64
	MemoryBytes  int64
	CoreFraction int64
	GPUs         int64

	ImageID       string
	DiskType      string
	DiskSizeBytes int64

	NAT              bool
	SecurityGroupIDs []string
	ServiceAccountID string
	Preemptible      bool
}

type Compute interface {
	// ListInstances возвращает все инстансы каталога (пагинация внутри).
	ListInstances(ctx context.Context, folderID string) ([]Instance, error)
	GetInstance(ctx context.Context, id string) (*Instance, error)
	// CreateInstance ждёт завершения операции: нехватка ресурсов зоны
	// приходит именно в её результате.
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error)
	// DeleteInstance операцию не ждёт.
	DeleteInstance(ctx context.Context, id string) error
	LatestImageByFamily(ctx context.Context, folderID, family string) (string, error)
	Close(ctx context.Context) error
}
