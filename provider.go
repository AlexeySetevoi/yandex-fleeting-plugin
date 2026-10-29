package yandex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	mrand "math/rand/v2"
	"path"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/sync/errgroup"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/yandex-fleeting-plugin/internal/yandexapi"
)

const (
	// сколько запросов на создание шлём одновременно
	createConcurrency = 5

	// на сколько размещение уходит в конец очереди после отказа
	placementCooldown = 10 * time.Minute

	// как часто Update повторяет удаление сломанной ВМ
	brokenDeleteRetry = time.Minute

	// пауза после отказа всех размещений, удваивается до backoffMax;
	// backoffMax же — сколько ждём исхода пробного инстанса
	backoffMin = 10 * time.Second
	backoffMax = 5 * time.Minute
)

// Порядок, в котором пробуются размещения для нового инстанса.
const (
	// ordered — всегда с первого из конфига, остальные только как фолбэк
	strategyOrdered = "ordered"
	// round_robin — каждый новый инстанс начинает со следующего размещения
	strategyRoundRobin = "round_robin"
	// random — со случайного
	strategyRandom = "random"
)

// подменяется в тестах
var newCompute = yandexapi.NewSDKCompute

// Placement — один вариант размещения инстанса, см. InstanceGroup.Placements.
type Placement struct {
	Zone       string `json:"zone"`
	SubnetID   string `json:"subnet_id"`
	PlatformID string `json:"platform_id"`
}

type InstanceGroup struct {
	// Name — префикс имён инстансов и значение label fleeting-group, по
	// которому плагин узнаёт свои машины в каталоге.
	Name string `json:"name"`

	FolderID string `json:"folder_id"`

	// ServiceAccountKeyFile — путь к authorized key (JSON) сервисного
	// аккаунта, можно задать и через YC_SERVICE_ACCOUNT_KEY_FILE. Если не
	// задан — берётся сервисный аккаунт VM, на которой запущен плагин.
	ServiceAccountKeyFile string `json:"service_account_key_file"`

	Zone       string `json:"zone"`
	SubnetID   string `json:"subnet_id"`
	PlatformID string `json:"platform_id"`

	// Placements — упорядоченный список вариантов размещения: если в зоне
	// или на платформе не хватило ресурсов, пробуем следующий. Подсеть
	// привязана к зоне, поэтому задаётся в каждом варианте. Взаимоисключающе
	// с Zone/SubnetID/PlatformID.
	Placements []Placement `json:"placements"`

	// PlacementStrategy — с какого размещения начинать: ordered (по
	// умолчанию), round_robin или random. Фолбэк при отказе идёт по всем
	// остальным размещениям по кругу.
	PlacementStrategy string `json:"placement_strategy"`

	Cores        int     `json:"cores"`
	MemoryGB     float64 `json:"memory_gb"`
	CoreFraction int     `json:"core_fraction"`
	GPUs         int     `json:"gpus"`

	// ImageID и ImageFamily взаимоисключающие. Семейство ищется в
	// ImageFolderID (по умолчанию standard-images) при каждом создании, так
	// что новые сборки образа подхватываются без правки конфига.
	ImageID       string `json:"image_id"`
	ImageFamily   string `json:"image_family"`
	ImageFolderID string `json:"image_folder_id"`

	DiskType   string `json:"disk_type"`
	DiskSizeGB int    `json:"disk_size_gb"`

	// NAT выдаёт инстансу публичный адрес. Адрес эфемерный и освобождается
	// вместе с машиной.
	NAT              bool     `json:"nat"`
	SecurityGroupIDs []string `json:"security_group_ids"`

	// ServiceAccountID привязывается к создаваемым инстансам.
	ServiceAccountID string `json:"service_account_id"`
	Preemptible      bool   `json:"preemptible"`

	Labels   map[string]string `json:"labels"`
	Metadata map[string]string `json:"metadata"`

	// UserData и UserDataFile взаимоисключающие: cloud-init для Linux,
	// "#ps1"-скрипт для Windows.
	UserData     string `json:"user_data"`
	UserDataFile string `json:"user_data_file"`

	log      hclog.Logger
	settings provider.Settings
	client   yandexapi.Compute

	placements      []Placement
	sshKeysMetadata string

	// За принятыми запросами на создание следят фоновые горутины: Increase
	// их не ждёт, иначе встаёт весь цикл раннера (он однопоточный).
	watchCtx    context.Context
	watchCancel context.CancelFunc
	watchers    sync.WaitGroup

	now            func() time.Time
	mu             sync.Mutex
	exhaustedUntil map[Placement]time.Time
	// следующее стартовое размещение для round_robin
	nextPlacement int

	// brokenDeletes — когда Update последний раз удалял сломанную ВМ,
	// warned — последний залогированный незнакомый статус; ключ — id инстанса
	brokenDeletes map[string]time.Time
	warned        map[string]string

	// Бэкоф: fleeting повторяет Increase каждые 2 с без паузы, и без неё
	// каждая попытка — инстанс и диск, которые тут же откатываются.
	// failures — отказы подряд, до retryAt в облако не ходим; пока не истёк
	// probeUntil, ждём исхода пробного инстанса; lastErr — для ответа раннеру.
	failures   int
	retryAt    time.Time
	probeUntil time.Time
	lastErr    error
}

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.settings = settings
	g.log = log.With("folder_id", g.FolderID, "name", g.Name)

	if err := g.validate(); err != nil {
		return provider.ProviderInfo{}, err
	}
	if err := g.populate(); err != nil {
		return provider.ProviderInfo{}, err
	}

	if g.ServiceAccountKeyFile == "" {
		g.log.Info("service_account_key_file is not set, using the service account of the host instance")
	}

	client, err := newCompute(ctx, g.ServiceAccountKeyFile)
	if err != nil {
		return provider.ProviderInfo{}, err
	}
	g.client = client

	if err := g.setupSSHKey(); err != nil {
		return provider.ProviderInfo{}, err
	}

	g.watchCtx, g.watchCancel = context.WithCancel(context.Background())
	g.exhaustedUntil = map[Placement]time.Time{}
	if g.now == nil {
		g.now = time.Now
	}

	return provider.ProviderInfo{
		ID:        path.Join("yandex", g.FolderID, g.Name),
		MaxSize:   math.MaxInt,
		Version:   Version.String(),
		BuildInfo: Version.BuildInfo(),
	}, nil
}

// Update отдаёт все ВМ группы.
//
// Инстанс в timeout или не возвращённый Update 5 циклов подряд taskscaler
// забывает без Decrease, и ВМ остаётся в каталоге навсегда. Поэтому сломанные
// ВМ удаляем здесь сами, а незнакомый статус не пропускаем.
func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	instances, err := g.client.ListInstances(ctx, g.FolderID)
	if err != nil {
		return fmt.Errorf("could not list instances: %w", err)
	}

	seen := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if instance.Labels[labelGroup] != g.Name {
			continue
		}
		seen[instance.ID] = struct{}{}

		if Broken(instance.Status) {
			g.deleteBroken(ctx, instance)
		}

		state, ok := MapStatus(instance.Status)
		if !ok {
			// держим в учёте, пока статус не прояснится
			g.warnUnrecognized(instance.ID, instance.Status)
			state = provider.StateCreating
		}
		g.log.Debug("instance status", "id", instance.ID, "status", instance.Status, "state", state)

		update(instance.ID, state)
	}
	g.forgetUnseen(seen)

	return nil
}

// deleteBroken удаляет ВМ, которая сама не вернётся, не чаще раза в
// brokenDeleteRetry: пока облако её не убрало, Update видит её снова. Ошибка
// только в лог — повторим на следующем цикле (например, при упоре в лимит
// активных операций).
func (g *InstanceGroup) deleteBroken(ctx context.Context, instance yandexapi.Instance) {
	now := g.now()

	g.mu.Lock()
	if at, ok := g.brokenDeletes[instance.ID]; ok && now.Sub(at) < brokenDeleteRetry {
		g.mu.Unlock()
		return
	}
	if g.brokenDeletes == nil {
		g.brokenDeletes = map[string]time.Time{}
	}
	g.brokenDeletes[instance.ID] = now
	g.mu.Unlock()

	g.log.Warn("instance is broken, deleting", "id", instance.ID, "instance_name", instance.Name, "status", instance.Status)

	if err := g.client.DeleteInstance(ctx, instance.ID); err != nil && !errors.Is(err, yandexapi.ErrNotFound) {
		g.log.Error("could not delete broken instance", "id", instance.ID, "error", err)
	}
}

func (g *InstanceGroup) warnUnrecognized(id, status string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.warned[id] == status {
		return
	}
	if g.warned == nil {
		g.warned = map[string]string{}
	}
	g.warned[id] = status

	g.log.Warn("unrecognized instance status, reporting as creating", "id", id, "status", status)
}

// forgetUnseen чистит учёт по ВМ, которых в каталоге больше нет.
func (g *InstanceGroup) forgetUnseen(seen map[string]struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for id := range g.brokenDeletes {
		if _, ok := seen[id]; !ok {
			delete(g.brokenDeletes, id)
		}
	}
	for id := range g.warned {
		if _, ok := seen[id]; !ok {
			delete(g.warned, id)
		}
	}
}

// Increase возвращается, как только облако приняло запросы: инстанс с этого
// момента виден в Update как creating. Ошибки квоты, прав и конфигурации
// приходят сразу и попадают в ответ; то, что выясняется позже (нехватка
// ресурсов зоны), ловит watch. Если отказали все запросы, следующие ждут
// паузы (см. admit).
func (g *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	delta, err := g.admit(delta)
	if delta == 0 {
		return 0, err
	}

	var (
		mu        sync.Mutex
		errs      []error
		succeeded int
	)

	var eg errgroup.Group
	eg.SetLimit(createConcurrency)

	for range delta {
		eg.Go(func() error {
			op, placement, err := g.createInstance(ctx)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
				return nil
			}

			g.log.Info("instance requested", "id", op.InstanceID(), "zone", placement.Zone, "platform_id", placement.PlatformID)
			g.watch(op, placement)
			succeeded++
			return nil
		})
	}
	_ = eg.Wait()

	err = errors.Join(errs...)
	// отменённый запрос раннера — не отказ облака
	if succeeded == 0 && ctx.Err() == nil {
		g.fail(err)
	}

	return succeeded, err
}

// admit решает, сколько инстансов можно запросить сейчас: во время паузы —
// ни одного, после неё — один пробный, пока какой-нибудь инстанс не поднимется.
func (g *InstanceGroup) admit(delta int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.failures == 0 {
		return delta, nil
	}

	now := g.now()
	if now.Before(g.retryAt) {
		return 0, fmt.Errorf("backing off after %d failed attempts, next attempt in %s: %w",
			g.failures, g.retryAt.Sub(now).Round(time.Second), g.lastErr)
	}
	if now.Before(g.probeUntil) {
		return 0, nil
	}

	g.probeUntil = now.Add(backoffMax)
	g.log.Info("requesting a probe instance after failures", "failures", g.failures)
	return 1, nil
}

// fail считает отказ и откладывает следующую попытку. Отказы внутри уже идущей
// паузы не считаются: пачка инстансов, упавших разом, — один отказ.
func (g *InstanceGroup) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if now.Before(g.retryAt) {
		return
	}

	g.failures++
	delay := backoffMax
	if shift := g.failures - 1; shift < 16 {
		delay = min(backoffMin<<shift, backoffMax)
	}
	g.retryAt = now.Add(delay)
	g.probeUntil = time.Time{}
	g.lastErr = err

	g.log.Warn("instance creation failed, backing off", "failures", g.failures, "for", delay, "error", err)
}

// succeed снимает бэкоф: инстанс поднялся, облако снова выдаёт машины.
func (g *InstanceGroup) succeed() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.failures > 0 {
		g.log.Info("instance created, backoff reset", "failures", g.failures)
	}
	g.failures = 0
	g.retryAt = time.Time{}
	g.probeUntil = time.Time{}
	g.lastErr = nil
}

// createInstance перебирает варианты размещения по порядку и переходит к
// следующему при любой ошибке: авария зоны приходит под разными кодами
// (ResourceExhausted, Unavailable, FailedPrecondition "Zone is down",
// PermissionDenied на подсеть), по коду её от ошибки конфигурации не отличить.
// Ошибка конфигурации повторится во всех вариантах и вернётся вместе с ними.
func (g *InstanceGroup) createInstance(ctx context.Context) (yandexapi.CreateOperation, Placement, error) {
	imageID := g.ImageID
	if g.ImageFamily != "" {
		var err error
		if imageID, err = g.client.LatestImageByFamily(ctx, g.ImageFolderID, g.ImageFamily); err != nil {
			return nil, Placement{}, fmt.Errorf("could not resolve image family %s/%s: %w", g.ImageFolderID, g.ImageFamily, err)
		}
	}

	var errs []error

	candidates := g.orderedPlacements()
	for i, p := range candidates {
		op, err := g.client.CreateInstance(ctx, yandexapi.CreateInstanceRequest{
			FolderID: g.FolderID,
			// имя новое на каждую попытку, чтобы не упереться в уникальность
			Name:             g.Name + "-" + randomSuffix(),
			Labels:           g.instanceLabels(),
			Metadata:         g.instanceMetadata(),
			Zone:             p.Zone,
			SubnetID:         p.SubnetID,
			PlatformID:       p.PlatformID,
			Cores:            int64(g.Cores),
			MemoryBytes:      int64(g.MemoryGB * gib),
			CoreFraction:     int64(g.CoreFraction),
			GPUs:             int64(g.GPUs),
			ImageID:          imageID,
			DiskType:         g.DiskType,
			DiskSizeBytes:    int64(g.DiskSizeGB) * gib,
			NAT:              g.NAT,
			SecurityGroupIDs: g.SecurityGroupIDs,
			ServiceAccountID: g.ServiceAccountID,
			Preemptible:      g.Preemptible,
		})
		if err == nil {
			return op, p, nil
		}

		errs = append(errs, fmt.Errorf("zone %s platform %s: %w", p.Zone, p.PlatformID, err))

		// отменённый запрос раннера — не отказ размещения
		if ctx.Err() != nil {
			break
		}
		g.deferPlacement(p)

		if i < len(candidates)-1 {
			g.log.Warn("placement failed, trying next placement", "zone", p.Zone, "platform_id", p.PlatformID, "error", err)
		}
	}

	return nil, Placement{}, fmt.Errorf("could not create instance: %w", errors.Join(errs...))
}

// deferPlacement отправляет отказавшее размещение в конец очереди на
// placementCooldown.
func (g *InstanceGroup) deferPlacement(p Placement) {
	g.mu.Lock()
	g.exhaustedUntil[p] = g.now().Add(placementCooldown)
	g.mu.Unlock()

	g.log.Warn("placement moved to the end of the list", "zone", p.Zone, "platform_id", p.PlatformID, "for", placementCooldown)
}

// orderedPlacements — размещения по кругу от стартового (см. PlacementStrategy),
// но те, что недавно отказали, идут последними: их пробуем, только если
// остальные отказали.
func (g *InstanceGroup) orderedPlacements() []Placement {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	n := len(g.placements)

	start := 0
	switch g.PlacementStrategy {
	case strategyRoundRobin:
		start = g.nextPlacement % n
		g.nextPlacement = (start + 1) % n
	case strategyRandom:
		start = mrand.IntN(n) //nolint:gosec // выбор зоны, не криптография
	}

	ordered := make([]Placement, 0, n)
	var exhausted []Placement
	for i := range n {
		p := g.placements[(start+i)%n]
		if until, ok := g.exhaustedUntil[p]; ok && now.Before(until) {
			exhausted = append(exhausted, p)
			continue
		}
		delete(g.exhaustedUntil, p)
		ordered = append(ordered, p)
	}

	return append(ordered, exhausted...)
}

// watch дожидается операции создания в фоне. Упавшую операцию облако
// откатывает само: инстанс пропадает из списка, и раннер запрашивает замену —
// к тому моменту размещение уже в конце очереди, а Increase на паузе (см. fail).
func (g *InstanceGroup) watch(op yandexapi.CreateOperation, p Placement) {
	g.watchers.Add(1)

	go func() {
		defer g.watchers.Done()

		instance, err := op.Wait(g.watchCtx)
		if err == nil {
			g.log.Info("instance created", "id", instance.ID, "instance_name", instance.Name)
			g.succeed()
			return
		}
		if g.watchCtx.Err() != nil {
			return // плагин останавливается, операция здесь ни при чём
		}

		g.log.Error("instance creation failed after the request was accepted", "id", op.InstanceID(), "zone", p.Zone, "platform_id", p.PlatformID, "error", err)

		g.deferPlacement(p)
		g.fail(fmt.Errorf("zone %s platform %s: %w", p.Zone, p.PlatformID, err))
	}()
}

func (g *InstanceGroup) instanceLabels() map[string]string {
	labels := maps.Clone(g.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelGroup] = g.Name
	labels[labelManagedBy] = NAME
	return labels
}

func (g *InstanceGroup) instanceMetadata() map[string]string {
	metadata := maps.Clone(g.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	if g.sshKeysMetadata != "" {
		metadata[metadataSSHKeys] = g.sshKeysMetadata
	}
	if g.UserData != "" {
		metadata[metadataUserData] = g.UserData
	}
	return metadata
}

func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	if len(instances) == 0 {
		return nil, nil
	}

	var errs []error
	deleted := make([]string, 0, len(instances))

	for _, id := range instances {
		if err := g.client.DeleteInstance(ctx, id); err != nil {
			if errors.Is(err, yandexapi.ErrNotFound) {
				g.log.Warn("tried to delete an instance that does not exist", "id", id)
				deleted = append(deleted, id)
				continue
			}

			errs = append(errs, fmt.Errorf("could not delete instance %s: %w", id, err))
			continue
		}

		deleted = append(deleted, id)
	}

	return deleted, errors.Join(errs...)
}

func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	info := provider.ConnectInfo{ConnectorConfig: g.settings.ConnectorConfig}

	instance, err := g.client.GetInstance(ctx, id)
	if err != nil {
		return info, fmt.Errorf("could not get instance: %w", err)
	}

	info.ID = id
	info.InternalAddr = instance.InternalIP
	info.ExternalAddr = instance.ExternalIP

	return info, nil
}

func (g *InstanceGroup) Heartbeat(_ context.Context, _ string) error {
	return nil
}

func (g *InstanceGroup) Suspend(_ context.Context, _ []string) ([]string, error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

func (g *InstanceGroup) Resume(_ context.Context, _ []string) ([]string, error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	if g.watchCancel != nil {
		g.watchCancel()
		g.watchers.Wait()
	}

	if g.client == nil {
		return nil
	}
	return g.client.Close(ctx)
}

func randomSuffix() string {
	b := make([]byte, 4)
	// crypto/rand.Read с Go 1.24 не возвращает ошибку
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
