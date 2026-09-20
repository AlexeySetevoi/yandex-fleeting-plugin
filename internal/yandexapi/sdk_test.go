package yandexapi

import (
	"errors"
	"fmt"
	"testing"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMapError(t *testing.T) {
	notFound := status.Error(codes.NotFound, "instance not found")
	if err := mapError(notFound); !errors.Is(err, ErrNotFound) || status.Code(err) != codes.NotFound {
		t.Fatalf("mapError(NotFound) = %v", err)
	}

	// так go-sdk заворачивает ошибку упавшей операции
	failedOp := fmt.Errorf("operation (id=op-1) failed: %w", status.Error(codes.ResourceExhausted, "zone is full"))
	if err := mapError(failedOp); !errors.Is(err, ErrResourceExhausted) {
		t.Fatalf("mapError(wrapped ResourceExhausted) = %v", err)
	}

	other := status.Error(codes.PermissionDenied, "denied")
	if err := mapError(other); err != other {
		t.Fatalf("mapError(PermissionDenied) = %v, want the error unchanged", err)
	}
}

func TestFromProto(t *testing.T) {
	got := fromProto(&compute.Instance{
		Id:     "id-1",
		Name:   "runner-aaaa1111",
		Status: compute.Instance_RUNNING,
		Labels: map[string]string{"fleeting-group": "runner"},
		NetworkInterfaces: []*compute.NetworkInterface{{
			PrimaryV4Address: &compute.PrimaryAddress{
				Address:     "10.0.0.5",
				OneToOneNat: &compute.OneToOneNat{Address: "203.0.113.7"},
			},
		}},
	})

	if got.ID != "id-1" || got.Status != StatusRunning || got.InternalIP != "10.0.0.5" || got.ExternalIP != "203.0.113.7" {
		t.Fatalf("fromProto() = %+v", got)
	}

	// без NAT и вовсе без интерфейсов
	if got := fromProto(&compute.Instance{Id: "id-2"}); got.InternalIP != "" || got.ExternalIP != "" {
		t.Fatalf("fromProto(no interfaces) = %+v", got)
	}
}
