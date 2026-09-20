package yandex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/yandex-fleeting-plugin/internal/yandexapi"
)

// fakeCompute — yandexapi.Compute в памяти. create решает судьбу запроса сразу
// (квота, права), opErr — судьбу уже принятой операции (нехватка ресурсов
// зоны); пока hold не закрыт, операции не завершаются. Все запросы на создание
// и удаление записываются.
type fakeCompute struct {
	mu sync.Mutex

	instances []yandexapi.Instance
	create    func(req yandexapi.CreateInstanceRequest) error
	opErr     func(req yandexapi.CreateInstanceRequest) error
	hold      chan struct{}
	deleteErr map[string]error
	listErr   error
	images    map[string]string

	created []yandexapi.CreateInstanceRequest
	deleted []string
	closed  bool
}

type fakeOperation struct {
	instance yandexapi.Instance
	err      error
	hold     chan struct{}
}

func (o *fakeOperation) InstanceID() string { return o.instance.ID }

func (o *fakeOperation) Wait(ctx context.Context) (*yandexapi.Instance, error) {
	if o.hold != nil {
		select {
		case <-o.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if o.err != nil {
		return nil, o.err
	}

	instance := o.instance
	instance.Status = yandexapi.StatusRunning
	return &instance, nil
}

func (f *fakeCompute) ListInstances(_ context.Context, _ string) ([]yandexapi.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances, f.listErr
}

func (f *fakeCompute) GetInstance(_ context.Context, id string) (*yandexapi.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, instance := range f.instances {
		if instance.ID == id {
			return &instance, nil
		}
	}
	return nil, fmt.Errorf("%w: instance %s", yandexapi.ErrNotFound, id)
}

func (f *fakeCompute) CreateInstance(_ context.Context, req yandexapi.CreateInstanceRequest) (yandexapi.CreateOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.created = append(f.created, req)
	if f.create != nil {
		if err := f.create(req); err != nil {
			return nil, err
		}
	}

	// как в облаке: принятый запрос сразу даёт инстанс в PROVISIONING
	instance := yandexapi.Instance{
		ID:     fmt.Sprintf("id-%d", len(f.created)),
		Name:   req.Name,
		Status: yandexapi.StatusProvisioning,
		Labels: req.Labels,
	}
	f.instances = append(f.instances, instance)

	op := &fakeOperation{instance: instance, hold: f.hold}
	if f.opErr != nil {
		op.err = f.opErr(req)
	}
	return op, nil
}

func (f *fakeCompute) DeleteInstance(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.deleteErr[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeCompute) LatestImageByFamily(_ context.Context, folderID, family string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, ok := f.images[folderID+"/"+family]
	if !ok {
		return "", fmt.Errorf("%w: image family %s/%s", yandexapi.ErrNotFound, folderID, family)
	}
	return id, nil
}

func (f *fakeCompute) Close(_ context.Context) error {
	f.closed = true
	return nil
}

func validGroup() *InstanceGroup {
	return &InstanceGroup{
		Name:       "runner",
		FolderID:   "folder-1",
		Zone:       "ru-central1-a",
		SubnetID:   "subnet-a",
		Cores:      2,
		MemoryGB:   4,
		ImageID:    "image-1",
		DiskSizeGB: 30,
	}
}

// initGroup прогоняет настоящий Init с подменённым клиентом.
func initGroup(t *testing.T, g *InstanceGroup, fake *fakeCompute, settings provider.Settings) {
	t.Helper()

	orig := newCompute
	newCompute = func(context.Context, string) (yandexapi.Compute, error) { return fake, nil }
	t.Cleanup(func() { newCompute = orig })

	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	// гасит фоновые горутины watch, чтобы они не переживали тест
	t.Cleanup(func() { _ = g.Shutdown(context.Background()) })
}

func TestInitProviderInfo(t *testing.T) {
	g := validGroup()

	orig := newCompute
	newCompute = func(context.Context, string) (yandexapi.Compute, error) { return &fakeCompute{}, nil }
	t.Cleanup(func() { newCompute = orig })

	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if info.ID != "yandex/folder-1/runner" {
		t.Fatalf("ProviderInfo.ID = %q", info.ID)
	}
	// раннер пишет их в лог при старте — по ним видно, какая сборка плагина стоит
	if info.Version == "" || info.BuildInfo == "" {
		t.Fatalf("ProviderInfo version = %q, build info = %q, want both set", info.Version, info.BuildInfo)
	}
}

func TestUpdateFiltersByLabel(t *testing.T) {
	fake := &fakeCompute{instances: []yandexapi.Instance{
		{ID: "a", Status: yandexapi.StatusRunning, Labels: map[string]string{labelGroup: "runner"}},
		{ID: "b", Status: yandexapi.StatusProvisioning, Labels: map[string]string{labelGroup: "runner"}},
		{ID: "c", Status: yandexapi.StatusRunning, Labels: map[string]string{labelGroup: "other"}},
		// имя похоже, но label нет — не наш
		{ID: "d", Name: "runner-aaaa1111", Status: yandexapi.StatusRunning},
		{ID: "e", Status: "SOMETHING_NEW", Labels: map[string]string{labelGroup: "runner"}},
		{ID: "f", Status: yandexapi.StatusStopped, Labels: map[string]string{labelGroup: "runner"}},
	}}

	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	got := map[string]provider.State{}
	if err := g.Update(context.Background(), func(instance string, state provider.State) {
		got[instance] = state
	}); err != nil {
		t.Fatalf("Update() = %v", err)
	}

	want := map[string]provider.State{
		"a": provider.StateRunning,
		"b": provider.StateCreating,
		"f": provider.StateTimeout,
	}
	if len(got) != len(want) {
		t.Fatalf("Update() reported %v, want %v", got, want)
	}
	for id, state := range want {
		if got[id] != state {
			t.Fatalf("Update() state[%s] = %q, want %q", id, got[id], state)
		}
	}
}

func TestIncreaseRequest(t *testing.T) {
	fake := &fakeCompute{images: map[string]string{"standard-images/ubuntu-2404-lts": "image-latest"}}

	g := validGroup()
	g.ImageID = ""
	g.ImageFamily = "ubuntu-2404-lts"
	g.MemoryGB = 0.5
	g.NAT = true
	g.Preemptible = true
	g.Labels = map[string]string{"env": "ci"}
	g.Metadata = map[string]string{"serial-port-enable": "1"}
	g.UserData = "#cloud-config\n"
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if err != nil || succeeded != 1 {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}

	req := fake.created[0]

	if !strings.HasPrefix(req.Name, "runner-") || len(req.Name) != len("runner-")+8 {
		t.Fatalf("instance name = %q", req.Name)
	}
	if req.ImageID != "image-latest" {
		t.Fatalf("ImageID = %q, want the one resolved from the family", req.ImageID)
	}
	if req.MemoryBytes != 512<<20 || req.DiskSizeBytes != 30<<30 {
		t.Fatalf("MemoryBytes = %d, DiskSizeBytes = %d", req.MemoryBytes, req.DiskSizeBytes)
	}
	if req.PlatformID != defaultPlatformID || req.DiskType != defaultDiskType || req.CoreFraction != 100 {
		t.Fatalf("defaults not applied: %+v", req)
	}
	if !req.NAT || !req.Preemptible {
		t.Fatalf("NAT = %v, Preemptible = %v", req.NAT, req.Preemptible)
	}

	wantLabels := map[string]string{"env": "ci", labelGroup: "runner", labelManagedBy: NAME}
	for k, v := range wantLabels {
		if req.Labels[k] != v {
			t.Fatalf("label %s = %q, want %q", k, req.Labels[k], v)
		}
	}

	if req.Metadata["serial-port-enable"] != "1" || req.Metadata[metadataUserData] != "#cloud-config\n" {
		t.Fatalf("metadata = %v", req.Metadata)
	}
	if !strings.HasPrefix(req.Metadata[metadataSSHKeys], "ubuntu:ssh-ed25519 ") {
		t.Fatalf("metadata ssh-keys = %q", req.Metadata[metadataSSHKeys])
	}
	if len(g.settings.Key) == 0 {
		t.Fatal("generated private key was not stored in connector settings")
	}
	if _, ok := g.Labels[labelGroup]; ok {
		t.Fatal("instanceLabels() modified the configured labels")
	}
}

func TestIncreaseWinRMHasNoSSHKey(t *testing.T) {
	fake := &fakeCompute{}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Protocol:             provider.ProtocolWinRM,
		Password:             "secret",
		UseStaticCredentials: true,
	}})

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if _, ok := fake.created[0].Metadata[metadataSSHKeys]; ok {
		t.Fatal("ssh-keys metadata set for a winrm group")
	}
	if g.settings.Username != "Administrator" {
		t.Fatalf("Username = %q", g.settings.Username)
	}
}

func TestIncreasePlacementFallback(t *testing.T) {
	fake := &fakeCompute{create: func(req yandexapi.CreateInstanceRequest) error {
		if req.Zone == "ru-central1-a" {
			return fmt.Errorf("%w: zone is full", yandexapi.ErrResourceExhausted)
		}
		return nil
	}}

	g := validGroup()
	g.Zone, g.SubnetID = "", ""
	g.Placements = []Placement{
		{Zone: "ru-central1-a", SubnetID: "subnet-a"},
		{Zone: "ru-central1-b", SubnetID: "subnet-b", PlatformID: "standard-v2"},
	}
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if err != nil || succeeded != 1 {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}
	if len(fake.created) != 2 {
		t.Fatalf("create attempts = %d, want 2", len(fake.created))
	}

	second := fake.created[1]
	if second.Zone != "ru-central1-b" || second.SubnetID != "subnet-b" || second.PlatformID != "standard-v2" {
		t.Fatalf("second attempt = %+v", second)
	}
	if fake.created[0].Name == second.Name {
		t.Fatal("instance name reused across attempts")
	}
}

func TestIncreaseNoFallbackOnOtherErrors(t *testing.T) {
	fake := &fakeCompute{create: func(yandexapi.CreateInstanceRequest) error {
		return errors.New("permission denied")
	}}

	g := validGroup()
	g.Zone, g.SubnetID = "", ""
	g.Placements = []Placement{
		{Zone: "ru-central1-a", SubnetID: "subnet-a"},
		{Zone: "ru-central1-b", SubnetID: "subnet-b"},
	}
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if err == nil || succeeded != 0 {
		t.Fatalf("Increase() = %d, %v, want an error", succeeded, err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("create attempts = %d, want 1", len(fake.created))
	}
}

func TestIncreasePartialSuccess(t *testing.T) {
	var calls int
	fake := &fakeCompute{}
	// create вызывается под мьютексом фейка
	fake.create = func(yandexapi.CreateInstanceRequest) error {
		calls++
		if calls%2 == 0 {
			return fmt.Errorf("%w: quota", yandexapi.ErrResourceExhausted)
		}
		return nil
	}

	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 8)
	if succeeded != 4 {
		t.Fatalf("Increase() succeeded = %d, want 4", succeeded)
	}
	if !errors.Is(err, yandexapi.ErrResourceExhausted) {
		t.Fatalf("Increase() error = %v", err)
	}
}

func TestIncreaseImageFamilyNotFound(t *testing.T) {
	fake := &fakeCompute{}
	g := validGroup()
	g.ImageID = ""
	g.ImageFamily = "no-such-family"
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if succeeded != 0 || !errors.Is(err, yandexapi.ErrNotFound) {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}
	if len(fake.created) != 0 {
		t.Fatal("instance creation attempted without an image")
	}
}

func TestDecrease(t *testing.T) {
	fake := &fakeCompute{deleteErr: map[string]error{
		"gone":   fmt.Errorf("%w: instance gone", yandexapi.ErrNotFound),
		"broken": errors.New("internal error"),
	}}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	deleted, err := g.Decrease(context.Background(), []string{"ok", "gone", "broken"})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("Decrease() error = %v", err)
	}
	if len(deleted) != 2 || deleted[0] != "ok" || deleted[1] != "gone" {
		t.Fatalf("Decrease() deleted = %v", deleted)
	}
}

func TestConnectInfo(t *testing.T) {
	fake := &fakeCompute{instances: []yandexapi.Instance{
		{ID: "a", InternalIP: "10.0.0.5", ExternalIP: "203.0.113.7"},
	}}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	info, err := g.ConnectInfo(context.Background(), "a")
	if err != nil {
		t.Fatalf("ConnectInfo() = %v", err)
	}
	if info.ID != "a" || info.InternalAddr != "10.0.0.5" || info.ExternalAddr != "203.0.113.7" {
		t.Fatalf("ConnectInfo() = %+v", info)
	}
	if info.Username != "ubuntu" || len(info.Key) == 0 {
		t.Fatalf("ConnectInfo() credentials: username=%q has_key=%v", info.Username, len(info.Key) > 0)
	}

	if _, err := g.ConnectInfo(context.Background(), "missing"); !errors.Is(err, yandexapi.ErrNotFound) {
		t.Fatalf("ConnectInfo(missing) = %v", err)
	}
}

func TestStaticSSHKey(t *testing.T) {
	pub, priv, err := generateSSHKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	g := validGroup()
	initGroup(t, g, &fakeCompute{}, provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Username:             "ci",
		Key:                  priv,
		UseStaticCredentials: true,
	}})

	if want := "ci:" + strings.TrimSpace(string(pub)); g.sshKeysMetadata != want {
		t.Fatalf("ssh-keys = %q, want %q", g.sshKeysMetadata, want)
	}
}

func TestShutdownClosesClient(t *testing.T) {
	fake := &fakeCompute{}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if !fake.closed {
		t.Fatal("client was not closed")
	}
}

func TestInitErrors(t *testing.T) {
	orig := newCompute
	t.Cleanup(func() { newCompute = orig })

	var built bool
	newCompute = func(context.Context, string) (yandexapi.Compute, error) {
		built = true
		return &fakeCompute{}, nil
	}

	t.Run("invalid config", func(t *testing.T) {
		g := validGroup()
		g.FolderID = ""
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
			t.Fatal("Init() = nil for an invalid config")
		}
		if built {
			t.Fatal("api client built although the config is invalid")
		}
	})

	t.Run("unreadable user data file", func(t *testing.T) {
		g := validGroup()
		g.UserDataFile = "/nonexistent/user-data.yml"
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
			t.Fatal("Init() = nil for a missing user_data_file")
		}
	})

	t.Run("invalid static ssh key", func(t *testing.T) {
		g := validGroup()
		settings := provider.Settings{ConnectorConfig: provider.ConnectorConfig{
			Key:                  []byte("not a key"),
			UseStaticCredentials: true,
		}}
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err == nil {
			t.Fatal("Init() = nil for an invalid static ssh key")
		}
	})

	t.Run("client cannot be built", func(t *testing.T) {
		newCompute = func(_ context.Context, keyFile string) (yandexapi.Compute, error) {
			return nil, fmt.Errorf("could not load service account key %s", keyFile)
		}

		g := validGroup()
		g.ServiceAccountKeyFile = "/etc/gitlab-runner/yc-key.json"
		_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
		if err == nil || !strings.Contains(err.Error(), "/etc/gitlab-runner/yc-key.json") {
			t.Fatalf("Init() = %v, want the key file error", err)
		}
	})
}

// Статический пароль по SSH: ключа нет, в metadata класть нечего.
func TestStaticSSHPasswordHasNoKey(t *testing.T) {
	fake := &fakeCompute{}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Password:             "secret",
		UseStaticCredentials: true,
	}})

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if _, ok := fake.created[0].Metadata[metadataSSHKeys]; ok {
		t.Fatal("ssh-keys metadata set without a key")
	}
	if len(g.settings.Key) != 0 {
		t.Fatal("a key was generated despite use_static_credentials")
	}
}

func TestUpdateListError(t *testing.T) {
	fake := &fakeCompute{listErr: errors.New("unavailable")}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	called := false
	err := g.Update(context.Background(), func(string, provider.State) { called = true })
	if err == nil || called {
		t.Fatalf("Update() = %v, callback called = %v", err, called)
	}
}

func TestDecreaseEmpty(t *testing.T) {
	g := validGroup()
	initGroup(t, g, &fakeCompute{}, provider.Settings{})

	deleted, err := g.Decrease(context.Background(), nil)
	if err != nil || len(deleted) != 0 {
		t.Fatalf("Decrease(nil) = %v, %v", deleted, err)
	}
}

func TestHeartbeatAndSuspendResume(t *testing.T) {
	g := validGroup()
	initGroup(t, g, &fakeCompute{}, provider.Settings{})

	if err := g.Heartbeat(context.Background(), "a"); err != nil {
		t.Fatalf("Heartbeat() = %v", err)
	}
	if _, err := g.Suspend(context.Background(), []string{"a"}); !errors.Is(err, provider.ErrSuspendResumeNotSupported) {
		t.Fatalf("Suspend() = %v", err)
	}
	if _, err := g.Resume(context.Background(), []string{"a"}); !errors.Is(err, provider.ErrSuspendResumeNotSupported) {
		t.Fatalf("Resume() = %v", err)
	}
}

// Shutdown зовут и после неудачного Init, когда клиента ещё нет.
func TestShutdownWithoutInit(t *testing.T) {
	if err := validGroup().Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

func TestRandomSuffix(t *testing.T) {
	a, b := randomSuffix(), randomSuffix()
	if len(a) != 8 || a == b {
		t.Fatalf("randomSuffix() = %q, %q", a, b)
	}
}

// Цикл раннера однопоточный: пока Increase не вернулся, не идут ни Update, ни
// удаление. Поэтому он обязан вернуться, не дожидаясь операций.
func TestIncreaseDoesNotWaitForOperations(t *testing.T) {
	fake := &fakeCompute{hold: make(chan struct{})}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	type result struct {
		succeeded int
		err       error
	}
	done := make(chan result, 1)
	go func() {
		succeeded, err := g.Increase(context.Background(), 3)
		done <- result{succeeded, err}
	}()

	select {
	case r := <-done:
		if r.err != nil || r.succeeded != 3 {
			t.Fatalf("Increase() = %d, %v", r.succeeded, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Increase() is blocked on unfinished create operations")
	}

	// принятые инстансы уже видны раннеру как creating
	creating := 0
	if err := g.Update(context.Background(), func(_ string, state provider.State) {
		if state == provider.StateCreating {
			creating++
		}
	}); err != nil || creating != 3 {
		t.Fatalf("Update() reported %d creating instances, err %v, want 3", creating, err)
	}

	close(fake.hold)
	g.watchers.Wait()
}

func asyncPlacementsGroup() *InstanceGroup {
	g := validGroup()
	g.Zone, g.SubnetID = "", ""
	g.Placements = []Placement{
		{Zone: "ru-central1-a", SubnetID: "subnet-a"},
		{Zone: "ru-central1-b", SubnetID: "subnet-b"},
	}
	return g
}

// Нехватка ресурсов зоны приходит уже после того, как запрос принят. Increase
// к этому моменту вернулся, поэтому fallback срабатывает на следующем запросе:
// размещение уходит в конец очереди, а через placementCooldown возвращается.
func TestAsyncExhaustionMovesPlacementToTheEnd(t *testing.T) {
	exhausted := true
	fake := &fakeCompute{}
	fake.opErr = func(req yandexapi.CreateInstanceRequest) error {
		if exhausted && req.Zone == "ru-central1-a" {
			return fmt.Errorf("%w: not enough resources in zone", yandexapi.ErrResourceExhausted)
		}
		return nil
	}

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	g := asyncPlacementsGroup()
	g.now = func() time.Time { return now }
	initGroup(t, g, fake, provider.Settings{})

	increase := func() string {
		t.Helper()
		succeeded, err := g.Increase(context.Background(), 1)
		if err != nil || succeeded != 1 {
			t.Fatalf("Increase() = %d, %v", succeeded, err)
		}
		g.watchers.Wait()
		return fake.created[len(fake.created)-1].Zone
	}

	if zone := increase(); zone != "ru-central1-a" {
		t.Fatalf("first request went to %s, want the first placement", zone)
	}
	if zone := increase(); zone != "ru-central1-b" {
		t.Fatalf("request after the async failure went to %s, want the next placement", zone)
	}

	exhausted = false
	now = now.Add(placementCooldown - time.Second)
	if zone := increase(); zone != "ru-central1-b" {
		t.Fatalf("request within the cooldown went to %s", zone)
	}

	now = now.Add(2 * time.Second)
	if zone := increase(); zone != "ru-central1-a" {
		t.Fatalf("request after the cooldown went to %s, want the first placement again", zone)
	}
}

// Любая другая ошибка операции от смены зоны не лечится: порядок не меняется.
func TestAsyncOtherErrorKeepsPlacementOrder(t *testing.T) {
	fake := &fakeCompute{opErr: func(yandexapi.CreateInstanceRequest) error {
		return errors.New("internal error")
	}}
	g := asyncPlacementsGroup()
	initGroup(t, g, fake, provider.Settings{})

	for range 2 {
		if succeeded, err := g.Increase(context.Background(), 1); err != nil || succeeded != 1 {
			t.Fatalf("Increase() = %d, %v", succeeded, err)
		}
		g.watchers.Wait()
	}

	for i, req := range fake.created {
		if req.Zone != "ru-central1-a" {
			t.Fatalf("request #%d went to %s, want the first placement", i+1, req.Zone)
		}
	}
}

// Размещения в конце очереди не выключены: если ресурсы кончились везде,
// пробуем все в порядке из конфига, а не отказываем раннеру без попытки.
func TestAllPlacementsExhaustedAreStillTried(t *testing.T) {
	fake := &fakeCompute{opErr: func(yandexapi.CreateInstanceRequest) error {
		return fmt.Errorf("%w: not enough resources", yandexapi.ErrResourceExhausted)
	}}
	g := asyncPlacementsGroup()
	initGroup(t, g, fake, provider.Settings{})

	var zones []string
	for range 3 {
		if succeeded, err := g.Increase(context.Background(), 1); err != nil || succeeded != 1 {
			t.Fatalf("Increase() = %d, %v", succeeded, err)
		}
		g.watchers.Wait()
		zones = append(zones, fake.created[len(fake.created)-1].Zone)
	}

	want := []string{"ru-central1-a", "ru-central1-b", "ru-central1-a"}
	if strings.Join(zones, ",") != strings.Join(want, ",") {
		t.Fatalf("zones tried = %v, want %v", zones, want)
	}
}

// Остановка плагина не должна висеть на операциях, которые ещё идут.
func TestShutdownDoesNotWaitForOperations(t *testing.T) {
	fake := &fakeCompute{hold: make(chan struct{})}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	if succeeded, err := g.Increase(context.Background(), 2); err != nil || succeeded != 2 {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}

	done := make(chan error, 1)
	go func() { done <- g.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown() is blocked on unfinished create operations")
	}

	// отменённое ожидание — не отказ зоны
	if got := g.orderedPlacements(); len(got) != 1 || len(g.exhaustedUntil) != 0 {
		t.Fatalf("placements after shutdown = %v, exhausted = %v", got, g.exhaustedUntil)
	}
}
