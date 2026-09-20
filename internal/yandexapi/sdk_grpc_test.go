package yandexapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/operation"
	"github.com/yandex-cloud/go-sdk/v2/credentials"
	"github.com/yandex-cloud/go-sdk/v2/pkg/endpoints"
	"github.com/yandex-cloud/go-sdk/v2/pkg/options"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

// fakeCloud — Compute API в памяти, поднятый настоящим gRPC-сервером: запросы
// проходят через go-sdk целиком (резолв эндпоинта, авторизация, опрос
// операций), как у timeweb через httptest.
type fakeCloud struct {
	mu sync.Mutex

	instances []*compute.Instance
	images    map[string]string

	// createErr — ошибка сразу в ответе на Create; operationErr — в результате
	// операции; pendingPolls — сколько опросов операция ещё "выполняется".
	listErr      error
	createErr    error
	operationErr error
	pendingPolls int

	listRequests   []*compute.ListInstancesRequest
	createRequests []*compute.CreateInstanceRequest
	deleteRequests []string
	polls          int
	authorization  []string
}

func (f *fakeCloud) List(_ context.Context, req *compute.ListInstancesRequest) (*compute.ListInstancesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.listRequests = append(f.listRequests, req)
	if f.listErr != nil {
		return nil, f.listErr
	}

	start := 0
	if req.GetPageToken() != "" {
		var err error
		if start, err = strconv.Atoi(req.GetPageToken()); err != nil {
			return nil, status.Error(codes.InvalidArgument, "bad page token")
		}
	}

	end := min(start+int(req.GetPageSize()), len(f.instances))
	resp := &compute.ListInstancesResponse{Instances: f.instances[start:end]}
	if end < len(f.instances) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	return resp, nil
}

func (f *fakeCloud) Get(ctx context.Context, req *compute.GetInstanceRequest) (*compute.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, instance := range f.instances {
		if instance.GetId() == req.GetInstanceId() {
			return instance, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "instance %s not found", req.GetInstanceId())
}

func (f *fakeCloud) Create(_ context.Context, req *compute.CreateInstanceRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.createRequests = append(f.createRequests, req)
	if f.createErr != nil {
		return nil, f.createErr
	}

	return f.createOperation(false), nil
}

// createOperation собирает операцию создания; вызывать под f.mu.
func (f *fakeCloud) createOperation(done bool) *operation.Operation {
	meta, err := anypb.New(&compute.CreateInstanceMetadata{InstanceId: "new-instance"})
	if err != nil {
		panic(err)
	}

	op := &operation.Operation{Id: "op-1", Metadata: meta, Done: done}
	if !done {
		return op
	}

	if f.operationErr != nil {
		op.Result = &operation.Operation_Error{Error: status.Convert(f.operationErr).Proto()}
		return op
	}

	req := f.createRequests[len(f.createRequests)-1]
	response, err := anypb.New(&compute.Instance{
		Id:     "new-instance",
		Name:   req.GetName(),
		Status: compute.Instance_RUNNING,
		Labels: req.GetLabels(),
		NetworkInterfaces: []*compute.NetworkInterface{{
			PrimaryV4Address: &compute.PrimaryAddress{Address: "10.0.0.9"},
		}},
	})
	if err != nil {
		panic(err)
	}
	op.Result = &operation.Operation_Response{Response: response}
	return op
}

func (f *fakeCloud) Delete(_ context.Context, req *compute.DeleteInstanceRequest) (*operation.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, instance := range f.instances {
		if instance.GetId() == req.GetInstanceId() {
			f.deleteRequests = append(f.deleteRequests, req.GetInstanceId())

			meta, err := anypb.New(&compute.DeleteInstanceMetadata{InstanceId: req.GetInstanceId()})
			if err != nil {
				panic(err)
			}
			// операция не завершена: DeleteInstance не должен её ждать
			return &operation.Operation{Id: "op-delete", Metadata: meta}, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "instance %s not found", req.GetInstanceId())
}

func (f *fakeCloud) GetLatestByFamily(_ context.Context, req *compute.GetImageLatestByFamilyRequest) (*compute.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, ok := f.images[req.GetFolderId()+"/"+req.GetFamily()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "image family %s not found", req.GetFamily())
	}
	return &compute.Image{Id: id, Family: req.GetFamily()}, nil
}

// Сервисы — отдельные типы-обёртки: у InstanceService, ImageService и
// OperationService методы Get с разными сигнатурами.
type operationService struct {
	operation.UnimplementedOperationServiceServer
	cloud *fakeCloud
}

func (s operationService) Get(_ context.Context, req *operation.GetOperationRequest) (*operation.Operation, error) {
	f := s.cloud
	f.mu.Lock()
	defer f.mu.Unlock()

	if req.GetOperationId() != "op-1" {
		return nil, status.Errorf(codes.NotFound, "operation %s not found", req.GetOperationId())
	}

	f.polls++
	return f.createOperation(f.polls > f.pendingPolls), nil
}

type imageService struct {
	compute.UnimplementedImageServiceServer
	cloud *fakeCloud
}

func (s imageService) GetLatestByFamily(ctx context.Context, req *compute.GetImageLatestByFamilyRequest) (*compute.Image, error) {
	return s.cloud.GetLatestByFamily(ctx, req)
}

type instanceService struct {
	compute.UnimplementedInstanceServiceServer
	cloud *fakeCloud
}

func (s instanceService) List(ctx context.Context, req *compute.ListInstancesRequest) (*compute.ListInstancesResponse, error) {
	return s.cloud.List(ctx, req)
}

func (s instanceService) Get(ctx context.Context, req *compute.GetInstanceRequest) (*compute.Instance, error) {
	return s.cloud.Get(ctx, req)
}

func (s instanceService) Create(ctx context.Context, req *compute.CreateInstanceRequest) (*operation.Operation, error) {
	return s.cloud.Create(ctx, req)
}

func (s instanceService) Delete(ctx context.Context, req *compute.DeleteInstanceRequest) (*operation.Operation, error) {
	return s.cloud.Delete(ctx, req)
}

// newTestCompute поднимает fakeCloud на localhost и возвращает sdkCompute,
// собранный тем же newSDKCompute, что и в бою, но смотрящий на этот сервер.
func newTestCompute(t *testing.T, cloud *fakeCloud) *sdkCompute {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			cloud.mu.Lock()
			cloud.authorization = append(cloud.authorization, md.Get("authorization")...)
			cloud.mu.Unlock()
			return handler(ctx, req)
		},
	))
	compute.RegisterInstanceServiceServer(server, instanceService{cloud: cloud})
	compute.RegisterImageServiceServer(server, imageService{cloud: cloud})
	operation.RegisterOperationServiceServer(server, operationService{cloud: cloud})

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	client, err := newSDKCompute(ctx,
		options.WithCredentials(credentials.IAMToken("test-token")),
		options.WithEndpointsResolver(endpoints.NewSingleEndpointResolver(
			listener.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)),
	)
	if err != nil {
		t.Fatalf("newSDKCompute() = %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	client.pollInterval = func(int) time.Duration { return 5 * time.Millisecond }

	return client
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSDKListInstancesPaginates(t *testing.T) {
	cloud := &fakeCloud{}
	for i := range 5 {
		cloud.instances = append(cloud.instances, &compute.Instance{
			Id:     fmt.Sprintf("id-%d", i),
			Status: compute.Instance_RUNNING,
			Labels: map[string]string{"fleeting-group": "runner"},
		})
	}

	client := newTestCompute(t, cloud)
	client.pageSize = 2

	instances, err := client.ListInstances(testContext(t), "folder-1")
	if err != nil {
		t.Fatalf("ListInstances() = %v", err)
	}
	if len(instances) != 5 {
		t.Fatalf("ListInstances() returned %d instances, want 5", len(instances))
	}
	for i, instance := range instances {
		if instance.ID != fmt.Sprintf("id-%d", i) || instance.Status != StatusRunning || instance.Labels["fleeting-group"] != "runner" {
			t.Fatalf("instances[%d] = %+v", i, instance)
		}
	}

	if len(cloud.listRequests) != 3 {
		t.Fatalf("list requests = %d, want 3 pages", len(cloud.listRequests))
	}
	for _, req := range cloud.listRequests {
		if req.GetFolderId() != "folder-1" || req.GetPageSize() != 2 {
			t.Fatalf("list request = %v", req)
		}
	}
}

func TestSDKListInstancesError(t *testing.T) {
	cloud := &fakeCloud{listErr: status.Error(codes.PermissionDenied, "denied")}
	client := newTestCompute(t, cloud)

	instances, err := client.ListInstances(testContext(t), "folder-1")
	if status.Code(err) != codes.PermissionDenied || instances != nil {
		t.Fatalf("ListInstances() = %v, %v, want PermissionDenied", instances, err)
	}
}

func TestSDKSendsIAMToken(t *testing.T) {
	cloud := &fakeCloud{}
	client := newTestCompute(t, cloud)

	if _, err := client.ListInstances(testContext(t), "folder-1"); err != nil {
		t.Fatalf("ListInstances() = %v", err)
	}
	if len(cloud.authorization) == 0 || cloud.authorization[0] != "Bearer test-token" {
		t.Fatalf("authorization metadata = %v, want Bearer test-token", cloud.authorization)
	}
}

func TestSDKGetInstance(t *testing.T) {
	cloud := &fakeCloud{instances: []*compute.Instance{{
		Id:     "id-1",
		Status: compute.Instance_STOPPED,
		NetworkInterfaces: []*compute.NetworkInterface{{
			PrimaryV4Address: &compute.PrimaryAddress{
				Address:     "10.0.0.5",
				OneToOneNat: &compute.OneToOneNat{Address: "203.0.113.7"},
			},
		}},
	}}}
	client := newTestCompute(t, cloud)

	instance, err := client.GetInstance(testContext(t), "id-1")
	if err != nil {
		t.Fatalf("GetInstance() = %v", err)
	}
	if instance.Status != StatusStopped || instance.InternalIP != "10.0.0.5" || instance.ExternalIP != "203.0.113.7" {
		t.Fatalf("GetInstance() = %+v", instance)
	}

	if _, err := client.GetInstance(testContext(t), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetInstance(missing) = %v, want ErrNotFound", err)
	}
}

func TestSDKCreateInstance(t *testing.T) {
	cloud := &fakeCloud{pendingPolls: 2}
	client := newTestCompute(t, cloud)

	op, err := client.CreateInstance(testContext(t), CreateInstanceRequest{
		FolderID:         "folder-1",
		Name:             "runner-aaaa1111",
		Labels:           map[string]string{"fleeting-group": "runner"},
		Metadata:         map[string]string{"ssh-keys": "ubuntu:ssh-ed25519 AAAA"},
		Zone:             "ru-central1-a",
		SubnetID:         "subnet-a",
		PlatformID:       "standard-v3",
		Cores:            2,
		MemoryBytes:      4 << 30,
		CoreFraction:     50,
		GPUs:             1,
		ImageID:          "image-1",
		DiskType:         "network-ssd",
		DiskSizeBytes:    30 << 30,
		NAT:              true,
		SecurityGroupIDs: []string{"sg-1"},
		ServiceAccountID: "sa-1",
		Preemptible:      true,
	})
	if err != nil {
		t.Fatalf("CreateInstance() = %v", err)
	}

	// запрос принят: id известен сразу, операцию никто не ждал
	if op.InstanceID() != "new-instance" {
		t.Fatalf("InstanceID() = %q", op.InstanceID())
	}
	if cloud.polls != 0 {
		t.Fatalf("CreateInstance() polled the operation %d times, want it to return right after the request is accepted", cloud.polls)
	}

	instance, err := op.Wait(testContext(t))
	if err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if instance.ID != "new-instance" || instance.Name != "runner-aaaa1111" || instance.InternalIP != "10.0.0.9" {
		t.Fatalf("Wait() = %+v", instance)
	}
	if cloud.polls != 3 {
		t.Fatalf("operation polls = %d, want 3 (two pending, one done)", cloud.polls)
	}

	req := cloud.createRequests[0]

	if req.GetFolderId() != "folder-1" || req.GetName() != "runner-aaaa1111" || req.GetZoneId() != "ru-central1-a" || req.GetPlatformId() != "standard-v3" {
		t.Fatalf("create request = %v", req)
	}
	if req.GetLabels()["fleeting-group"] != "runner" || req.GetMetadata()["ssh-keys"] != "ubuntu:ssh-ed25519 AAAA" {
		t.Fatalf("labels = %v, metadata = %v", req.GetLabels(), req.GetMetadata())
	}

	resources := req.GetResourcesSpec()
	if resources.GetCores() != 2 || resources.GetMemory() != 4<<30 || resources.GetCoreFraction() != 50 || resources.GetGpus() != 1 {
		t.Fatalf("resources = %v", resources)
	}

	disk := req.GetBootDiskSpec()
	if !disk.GetAutoDelete() {
		t.Fatal("boot disk is not auto_delete: it would outlive the instance")
	}
	if spec := disk.GetDiskSpec(); spec.GetImageId() != "image-1" || spec.GetTypeId() != "network-ssd" || spec.GetSize() != 30<<30 {
		t.Fatalf("boot disk spec = %v", spec)
	}

	if len(req.GetNetworkInterfaceSpecs()) != 1 {
		t.Fatalf("network interfaces = %v", req.GetNetworkInterfaceSpecs())
	}
	nic := req.GetNetworkInterfaceSpecs()[0]
	if nic.GetSubnetId() != "subnet-a" || len(nic.GetSecurityGroupIds()) != 1 || nic.GetSecurityGroupIds()[0] != "sg-1" {
		t.Fatalf("network interface = %v", nic)
	}
	if nat := nic.GetPrimaryV4AddressSpec().GetOneToOneNatSpec(); nat == nil || nat.GetIpVersion() != compute.IpVersion_IPV4 {
		t.Fatalf("one-to-one nat spec = %v", nat)
	}

	if !req.GetSchedulingPolicy().GetPreemptible() || req.GetServiceAccountId() != "sa-1" {
		t.Fatalf("preemptible = %v, service account = %q", req.GetSchedulingPolicy().GetPreemptible(), req.GetServiceAccountId())
	}
}

func TestSDKCreateInstanceWithoutNAT(t *testing.T) {
	cloud := &fakeCloud{}
	client := newTestCompute(t, cloud)

	if _, err := client.CreateInstance(testContext(t), CreateInstanceRequest{Name: "runner-bbbb2222", SubnetID: "subnet-a"}); err != nil {
		t.Fatalf("CreateInstance() = %v", err)
	}

	address := cloud.createRequests[0].GetNetworkInterfaceSpecs()[0].GetPrimaryV4AddressSpec()
	if address == nil {
		t.Fatal("primary v4 address spec is missing: the instance would get no internal address")
	}
	if address.GetOneToOneNatSpec() != nil {
		t.Fatal("one-to-one nat requested although NAT is off")
	}
}

// Нехватка ресурсов зоны приходит в результате операции, а не в ответе на
// Create — поэтому её отдаёт Wait, а сам запрос считается принятым.
func TestSDKCreateInstanceOperationFails(t *testing.T) {
	cloud := &fakeCloud{
		pendingPolls: 1,
		operationErr: status.Error(codes.ResourceExhausted, "not enough resources in zone"),
	}
	client := newTestCompute(t, cloud)

	op, err := client.CreateInstance(testContext(t), CreateInstanceRequest{Name: "runner-cccc3333"})
	if err != nil {
		t.Fatalf("CreateInstance() = %v, want the request to be accepted", err)
	}

	if _, err := op.Wait(testContext(t)); !errors.Is(err, ErrResourceExhausted) {
		t.Fatalf("Wait() = %v, want ErrResourceExhausted", err)
	}
}

func TestSDKCreateInstanceRequestFails(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		sentinel error
	}{
		{name: "quota", err: status.Error(codes.ResourceExhausted, "quota exceeded"), sentinel: ErrResourceExhausted},
		{name: "permission denied", err: status.Error(codes.PermissionDenied, "denied")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cloud := &fakeCloud{createErr: tt.err}
			client := newTestCompute(t, cloud)

			_, err := client.CreateInstance(testContext(t), CreateInstanceRequest{Name: "runner-dddd4444"})
			if err == nil {
				t.Fatal("CreateInstance() = nil, want an error")
			}
			if status.Code(err) != status.Code(tt.err) {
				t.Fatalf("CreateInstance() code = %s, want %s", status.Code(err), status.Code(tt.err))
			}
			if tt.sentinel != nil && !errors.Is(err, tt.sentinel) {
				t.Fatalf("CreateInstance() = %v, want %v", err, tt.sentinel)
			}
			if errors.Is(err, ErrResourceExhausted) != errors.Is(tt.sentinel, ErrResourceExhausted) {
				t.Fatalf("CreateInstance() = %v: would trigger a placement fallback it should not", err)
			}
			if cloud.polls != 0 {
				t.Fatal("operation polled although Create itself failed")
			}
		})
	}
}

func TestSDKCreateOperationWaitCancelled(t *testing.T) {
	cloud := &fakeCloud{pendingPolls: 1 << 30}
	client := newTestCompute(t, cloud)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	op, err := client.CreateInstance(testContext(t), CreateInstanceRequest{Name: "runner-eeee5555"})
	if err != nil {
		t.Fatalf("CreateInstance() = %v", err)
	}
	if _, err := op.Wait(ctx); err == nil {
		t.Fatal("Wait() = nil although the operation never finished")
	}
}

func TestSDKDeleteInstance(t *testing.T) {
	cloud := &fakeCloud{instances: []*compute.Instance{{Id: "id-1"}}}
	client := newTestCompute(t, cloud)

	if err := client.DeleteInstance(testContext(t), "id-1"); err != nil {
		t.Fatalf("DeleteInstance() = %v", err)
	}
	if len(cloud.deleteRequests) != 1 || cloud.deleteRequests[0] != "id-1" {
		t.Fatalf("delete requests = %v", cloud.deleteRequests)
	}
	if cloud.polls != 0 {
		t.Fatal("DeleteInstance waited for the operation")
	}

	if err := client.DeleteInstance(testContext(t), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteInstance(missing) = %v, want ErrNotFound", err)
	}
}

func TestSDKLatestImageByFamily(t *testing.T) {
	cloud := &fakeCloud{images: map[string]string{"standard-images/ubuntu-2404-lts": "image-42"}}
	client := newTestCompute(t, cloud)

	id, err := client.LatestImageByFamily(testContext(t), "standard-images", "ubuntu-2404-lts")
	if err != nil || id != "image-42" {
		t.Fatalf("LatestImageByFamily() = %q, %v", id, err)
	}

	if _, err := client.LatestImageByFamily(testContext(t), "standard-images", "no-such-family"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestImageByFamily(missing) = %v, want ErrNotFound", err)
	}
}

func TestNewSDKComputeBadKeyFile(t *testing.T) {
	if _, err := NewSDKCompute(context.Background(), "/nonexistent/yc-key.json"); err == nil {
		t.Fatal("NewSDKCompute() = nil error for a missing key file")
	}
}
