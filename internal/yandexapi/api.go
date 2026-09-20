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

// CreateOperation — принятый облаком запрос на создание инстанса.
type CreateOperation interface {
	// InstanceID известен сразу: инстанс уже виден в списке как PROVISIONING.
	InstanceID() string
	// Wait ждёт конца операции. Нехватка ресурсов зоны приходит именно здесь,
	// а не в ответе на запрос.
	Wait(ctx context.Context) (*Instance, error)
}

type Compute interface {
	// ListInstances возвращает все инстансы каталога (пагинация внутри).
	ListInstances(ctx context.Context, folderID string) ([]Instance, error)
	GetInstance(ctx context.Context, id string) (*Instance, error)
	// CreateInstance возвращается, как только облако приняло запрос; ошибки
	// квоты, прав и валидации приходят сразу, остальное — в CreateOperation.Wait.
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (CreateOperation, error)
	// DeleteInstance операцию не ждёт.
	DeleteInstance(ctx context.Context, id string) error
	LatestImageByFamily(ctx context.Context, folderID, family string) (string, error)
	Close(ctx context.Context) error
}
