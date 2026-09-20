package yandex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

const (
	labelGroup     = "fleeting-group"
	labelManagedBy = "managed-by"

	metadataSSHKeys  = "ssh-keys"
	metadataUserData = "user-data"

	defaultPlatformID    = "standard-v3"
	defaultImageFolderID = "standard-images"
	defaultDiskType      = "network-ssd"

	keyFileEnv = "YC_SERVICE_ACCOUNT_KEY_FILE"

	// имя инстанса — <name>-<8 hex>, лимит Compute на имя — 63 символа
	maxNameLength = 63 - 1 - 8

	gib = 1 << 30
)

// имя идёт и в имя инстанса, и в значение label
var nameRegexp = regexp.MustCompile(`^[a-z][-a-z0-9]*$`)

// UnmarshalJSON разбирает plugin_config строго. Библиотека fleeting делает
// обычный json.Unmarshal, который молча пропускает незнакомые ключи: опечатка
// в необязательном поле (preemtible, security_groups_ids) осталась бы
// незамеченной и стоила бы денег или доступа. Лучше упасть на старте.
func (g *InstanceGroup) UnmarshalJSON(data []byte) error {
	type plain InstanceGroup

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode((*plain)(g)); err != nil {
		return fmt.Errorf("invalid plugin config: %w", err)
	}
	return nil
}

func (g *InstanceGroup) validate() error {
	var errs []error

	if g.settings.Protocol == "" {
		g.settings.Protocol = provider.ProtocolSSH
	}

	switch g.settings.Protocol {
	case provider.ProtocolSSH:
		if g.settings.Username == "" {
			// на стандартных образах вход под root закрыт
			g.settings.Username = "ubuntu"
		}
	case provider.ProtocolWinRM, provider.ProtocolWinRMHttps:
		if g.settings.Username == "" {
			g.settings.Username = "Administrator"
		}
		// пароль администратора Compute API не выдаёт
		if !g.settings.UseStaticCredentials {
			errs = append(errs, errors.New("winrm requires connector config use_static_credentials = true: yandex cloud does not generate instance passwords"))
		}
	default:
		errs = append(errs, fmt.Errorf("unsupported connector config protocol: %s", g.settings.Protocol))
	}

	if g.Name == "" {
		errs = append(errs, errors.New("missing required plugin config: name"))
	} else if !nameRegexp.MatchString(g.Name) || len(g.Name) > maxNameLength {
		errs = append(errs, fmt.Errorf("invalid plugin config name %q: must match %s and be at most %d characters", g.Name, nameRegexp, maxNameLength))
	}
	if g.FolderID == "" {
		errs = append(errs, errors.New("missing required plugin config: folder_id"))
	}

	if len(g.Placements) > 0 {
		if g.Zone != "" {
			errs = append(errs, errors.New("mutually exclusive plugin config provided: zone, placements"))
		}
		if g.SubnetID != "" {
			errs = append(errs, errors.New("mutually exclusive plugin config provided: subnet_id, placements"))
		}
		if g.PlatformID != "" {
			errs = append(errs, errors.New("mutually exclusive plugin config provided: platform_id, placements"))
		}
		for i, p := range g.Placements {
			if p.Zone == "" {
				errs = append(errs, fmt.Errorf("missing required plugin config: placements[%d].zone", i))
			}
			if p.SubnetID == "" {
				errs = append(errs, fmt.Errorf("missing required plugin config: placements[%d].subnet_id", i))
			}
		}
	} else {
		if g.Zone == "" {
			errs = append(errs, errors.New("missing required plugin config: zone"))
		}
		if g.SubnetID == "" {
			errs = append(errs, errors.New("missing required plugin config: subnet_id"))
		}
	}

	if g.Cores <= 0 {
		errs = append(errs, errors.New("missing required plugin config: cores"))
	}
	if g.MemoryGB <= 0 {
		errs = append(errs, errors.New("missing required plugin config: memory_gb"))
	}
	if g.CoreFraction < 0 || g.CoreFraction > 100 {
		errs = append(errs, fmt.Errorf("invalid plugin config core_fraction: %d", g.CoreFraction))
	}
	if g.DiskSizeGB <= 0 {
		errs = append(errs, errors.New("missing required plugin config: disk_size_gb"))
	}

	if g.ImageID != "" && g.ImageFamily != "" {
		errs = append(errs, errors.New("mutually exclusive plugin config provided: image_id, image_family"))
	}
	if g.ImageID == "" && g.ImageFamily == "" {
		errs = append(errs, errors.New("missing required plugin config: image_id or image_family"))
	}
	if g.ImageFolderID != "" && g.ImageFamily == "" {
		errs = append(errs, errors.New("plugin config image_folder_id requires image_family"))
	}

	if g.UserData != "" && g.UserDataFile != "" {
		errs = append(errs, errors.New("mutually exclusive plugin config provided: user_data, user_data_file"))
	}

	for _, key := range []string{labelGroup, labelManagedBy} {
		if _, ok := g.Labels[key]; ok {
			errs = append(errs, fmt.Errorf("reserved key in plugin config labels: %s", key))
		}
	}
	for _, key := range []string{metadataSSHKeys, metadataUserData} {
		if _, ok := g.Metadata[key]; ok {
			errs = append(errs, fmt.Errorf("reserved key in plugin config metadata: %s (use connector config / user_data instead)", key))
		}
	}

	return errors.Join(errs...)
}

func (g *InstanceGroup) populate() error {
	if value := os.Getenv(keyFileEnv); value != "" {
		g.ServiceAccountKeyFile = value
	}

	if g.UserDataFile != "" {
		data, err := os.ReadFile(g.UserDataFile)
		if err != nil {
			return fmt.Errorf("failed to read user_data_file: %w", err)
		}
		g.UserData = string(data)
	}

	if g.CoreFraction == 0 {
		g.CoreFraction = 100
	}
	if g.DiskType == "" {
		g.DiskType = defaultDiskType
	}
	if g.ImageFamily != "" && g.ImageFolderID == "" {
		g.ImageFolderID = defaultImageFolderID
	}

	if len(g.Placements) == 0 {
		g.placements = []Placement{{Zone: g.Zone, SubnetID: g.SubnetID, PlatformID: g.PlatformID}}
	} else {
		g.placements = append([]Placement(nil), g.Placements...)
	}
	for i := range g.placements {
		if g.placements[i].PlatformID == "" {
			g.placements[i].PlatformID = defaultPlatformID
		}
	}

	return nil
}
