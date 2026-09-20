package yandexapi

import (
	"context"
	"fmt"
	"time"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	computesdk "github.com/yandex-cloud/go-sdk/services/compute/v1"
	ycsdk "github.com/yandex-cloud/go-sdk/v2"
	"github.com/yandex-cloud/go-sdk/v2/credentials"
	sdkop "github.com/yandex-cloud/go-sdk/v2/pkg/operation"
	"github.com/yandex-cloud/go-sdk/v2/pkg/options"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	listPageSize = 1000
	// как часто опрашиваем операцию создания
	operationPollInterval = time.Second
)

type sdkCompute struct {
	sdk       *ycsdk.SDK
	instances computesdk.InstanceClient
	images    computesdk.ImageClient

	pageSize     int64
	pollInterval sdkop.PollIntervalFunc
}

// NewSDKCompute: keyFile — authorized key сервисного аккаунта; пустой — берём
// сервисный аккаунт, привязанный к VM, на которой запущен плагин.
func NewSDKCompute(ctx context.Context, keyFile string) (Compute, error) {
	creds := credentials.Credentials(credentials.InstanceServiceAccount())
	if keyFile != "" {
		var err error
		if creds, err = credentials.ServiceAccountKeyFile(keyFile); err != nil {
			return nil, fmt.Errorf("could not load service account key: %w", err)
		}
	}

	return newSDKCompute(ctx, options.WithCredentials(creds))
}

func newSDKCompute(ctx context.Context, opts ...options.Option) (*sdkCompute, error) {
	sdk, err := ycsdk.Build(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("could not build yandex cloud sdk: %w", err)
	}

	return &sdkCompute{
		sdk:          sdk,
		instances:    computesdk.NewInstanceClient(sdk),
		images:       computesdk.NewImageClient(sdk),
		pageSize:     listPageSize,
		pollInterval: func(int) time.Duration { return operationPollInterval },
	}, nil
}

func (c *sdkCompute) ListInstances(ctx context.Context, folderID string) ([]Instance, error) {
	var result []Instance

	req := &compute.ListInstancesRequest{FolderId: folderID, PageSize: c.pageSize}
	for {
		resp, err := c.instances.List(ctx, req)
		if err != nil {
			return nil, mapError(err)
		}

		for _, instance := range resp.GetInstances() {
			result = append(result, fromProto(instance))
		}

		if resp.GetNextPageToken() == "" {
			return result, nil
		}
		req.PageToken = resp.GetNextPageToken()
	}
}

func (c *sdkCompute) GetInstance(ctx context.Context, id string) (*Instance, error) {
	instance, err := c.instances.Get(ctx, &compute.GetInstanceRequest{InstanceId: id})
	if err != nil {
		return nil, mapError(err)
	}

	result := fromProto(instance)
	return &result, nil
}

func (c *sdkCompute) CreateInstance(ctx context.Context, req CreateInstanceRequest) (CreateOperation, error) {
	address := &compute.PrimaryAddressSpec{}
	if req.NAT {
		address.OneToOneNatSpec = &compute.OneToOneNatSpec{IpVersion: compute.IpVersion_IPV4}
	}

	op, err := c.instances.Create(ctx, &compute.CreateInstanceRequest{
		FolderId:   req.FolderID,
		Name:       req.Name,
		Labels:     req.Labels,
		Metadata:   req.Metadata,
		ZoneId:     req.Zone,
		PlatformId: req.PlatformID,
		ResourcesSpec: &compute.ResourcesSpec{
			Cores:        req.Cores,
			Memory:       req.MemoryBytes,
			CoreFraction: req.CoreFraction,
			Gpus:         req.GPUs,
		},
		BootDiskSpec: &compute.AttachedDiskSpec{
			AutoDelete: true,
			Disk: &compute.AttachedDiskSpec_DiskSpec_{
				DiskSpec: &compute.AttachedDiskSpec_DiskSpec{
					TypeId: req.DiskType,
					Size:   req.DiskSizeBytes,
					Source: &compute.AttachedDiskSpec_DiskSpec_ImageId{ImageId: req.ImageID},
				},
			},
		},
		NetworkInterfaceSpecs: []*compute.NetworkInterfaceSpec{{
			SubnetId:             req.SubnetID,
			PrimaryV4AddressSpec: address,
			SecurityGroupIds:     req.SecurityGroupIDs,
		}},
		SchedulingPolicy: &compute.SchedulingPolicy{Preemptible: req.Preemptible},
		ServiceAccountId: req.ServiceAccountID,
	})
	if err != nil {
		return nil, mapError(err)
	}

	return &sdkCreateOperation{op: op, pollInterval: c.pollInterval}, nil
}

type sdkCreateOperation struct {
	op           *computesdk.InstanceCreateOperation
	pollInterval sdkop.PollIntervalFunc
}

func (o *sdkCreateOperation) InstanceID() string {
	return o.op.Metadata().GetInstanceId()
}

func (o *sdkCreateOperation) Wait(ctx context.Context) (*Instance, error) {
	instance, err := o.op.WaitInterval(ctx, o.pollInterval)
	if err != nil {
		return nil, mapError(err)
	}

	result := fromProto(instance)
	return &result, nil
}

func (c *sdkCompute) DeleteInstance(ctx context.Context, id string) error {
	if _, err := c.instances.Delete(ctx, &compute.DeleteInstanceRequest{InstanceId: id}); err != nil {
		return mapError(err)
	}
	return nil
}

func (c *sdkCompute) LatestImageByFamily(ctx context.Context, folderID, family string) (string, error) {
	image, err := c.images.GetLatestByFamily(ctx, &compute.GetImageLatestByFamilyRequest{
		FolderId: folderID,
		Family:   family,
	})
	if err != nil {
		return "", mapError(err)
	}
	return image.GetId(), nil
}

func (c *sdkCompute) Close(ctx context.Context) error {
	return c.sdk.Shutdown(ctx)
}

// mapError добавляет к gRPC-ошибке наш sentinel, исходная остаётся в цепочке.
func mapError(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case codes.ResourceExhausted:
		return fmt.Errorf("%w: %w", ErrResourceExhausted, err)
	default:
		return err
	}
}

func fromProto(instance *compute.Instance) Instance {
	result := Instance{
		ID:     instance.GetId(),
		Name:   instance.GetName(),
		Status: instance.GetStatus().String(),
		Labels: instance.GetLabels(),
	}

	if interfaces := instance.GetNetworkInterfaces(); len(interfaces) > 0 {
		primary := interfaces[0].GetPrimaryV4Address()
		result.InternalIP = primary.GetAddress()
		result.ExternalIP = primary.GetOneToOneNat().GetAddress()
	}

	return result
}
