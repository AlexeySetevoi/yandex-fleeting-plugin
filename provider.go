package yandex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"sync"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/sync/errgroup"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/yandex-fleeting-plugin/internal/yandexapi"
)

// сколько инстансов создаём одновременно: каждое создание ждёт свою операцию
const createConcurrency = 5

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

	return provider.ProviderInfo{
		ID:      path.Join("yandex", g.FolderID, g.Name),
		MaxSize: math.MaxInt,
	}, nil
}

func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	instances, err := g.client.ListInstances(ctx, g.FolderID)
	if err != nil {
		return fmt.Errorf("could not list instances: %w", err)
	}

	for _, instance := range instances {
		if instance.Labels[labelGroup] != g.Name {
			continue
		}

		state, ok := MapStatus(instance.Status)
		if !ok {
			g.log.Warn("unrecognized instance status, skipping", "id", instance.ID, "status", instance.Status)
			continue
		}
		g.log.Debug("instance status", "id", instance.ID, "status", instance.Status, "state", state)

		update(instance.ID, state)
	}

	return nil
}

func (g *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	var (
		mu        sync.Mutex
		errs      []error
		succeeded int
	)

	var eg errgroup.Group
	eg.SetLimit(createConcurrency)

	for range delta {
		eg.Go(func() error {
			instance, err := g.createInstance(ctx)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
				return nil
			}

			g.log.Info("instance created", "id", instance.ID, "name", instance.Name)
			succeeded++
			return nil
		})
	}
	_ = eg.Wait()

	return succeeded, errors.Join(errs...)
}

// createInstance перебирает варианты размещения по порядку; к следующему
// переходит только когда не хватило ресурсов или квоты, любая другая ошибка
// от смены зоны не вылечится.
func (g *InstanceGroup) createInstance(ctx context.Context) (*yandexapi.Instance, error) {
	imageID := g.ImageID
	if g.ImageFamily != "" {
		var err error
		if imageID, err = g.client.LatestImageByFamily(ctx, g.ImageFolderID, g.ImageFamily); err != nil {
			return nil, fmt.Errorf("could not resolve image family %s/%s: %w", g.ImageFolderID, g.ImageFamily, err)
		}
	}

	var errs []error

	for i, p := range g.placements {
		instance, err := g.client.CreateInstance(ctx, yandexapi.CreateInstanceRequest{
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
			return instance, nil
		}

		errs = append(errs, fmt.Errorf("zone %s platform %s: %w", p.Zone, p.PlatformID, err))

		if i < len(g.placements)-1 && errors.Is(err, yandexapi.ErrResourceExhausted) {
			g.log.Warn("not enough resources, trying next placement", "zone", p.Zone, "platform_id", p.PlatformID, "error", err)
			continue
		}

		break
	}

	return nil, fmt.Errorf("could not create instance: %w", errors.Join(errs...))
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
