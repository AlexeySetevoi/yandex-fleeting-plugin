package yandex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func TestValidate(t *testing.T) {
	winrm := provider.Settings{ConnectorConfig: provider.ConnectorConfig{Protocol: provider.ProtocolWinRM}}

	tests := []struct {
		name     string
		mutate   func(g *InstanceGroup)
		settings provider.Settings
		wantErr  string // пусто — ошибки быть не должно
	}{
		{name: "valid"},
		{name: "no name", mutate: func(g *InstanceGroup) { g.Name = "" }, wantErr: "plugin config: name"},
		{name: "bad name", mutate: func(g *InstanceGroup) { g.Name = "Runner_1" }, wantErr: "invalid plugin config name"},
		{name: "long name", mutate: func(g *InstanceGroup) { g.Name = strings.Repeat("a", maxNameLength+1) }, wantErr: "invalid plugin config name"},
		{name: "no folder", mutate: func(g *InstanceGroup) { g.FolderID = "" }, wantErr: "folder_id"},
		{name: "no zone", mutate: func(g *InstanceGroup) { g.Zone = "" }, wantErr: "plugin config: zone"},
		{name: "no subnet", mutate: func(g *InstanceGroup) { g.SubnetID = "" }, wantErr: "plugin config: subnet_id"},
		{name: "no cores", mutate: func(g *InstanceGroup) { g.Cores = 0 }, wantErr: "cores"},
		{name: "no memory", mutate: func(g *InstanceGroup) { g.MemoryGB = 0 }, wantErr: "memory_gb"},
		{name: "no disk size", mutate: func(g *InstanceGroup) { g.DiskSizeGB = 0 }, wantErr: "disk_size_gb"},
		{name: "bad core fraction", mutate: func(g *InstanceGroup) { g.CoreFraction = 150 }, wantErr: "core_fraction"},
		{name: "no image", mutate: func(g *InstanceGroup) { g.ImageID = "" }, wantErr: "image_id or image_family"},
		{name: "both images", mutate: func(g *InstanceGroup) { g.ImageFamily = "ubuntu-2404-lts" }, wantErr: "image_id, image_family"},
		{name: "image folder without family", mutate: func(g *InstanceGroup) { g.ImageFolderID = "folder-2" }, wantErr: "image_folder_id requires image_family"},
		{
			name:    "placements with zone",
			mutate:  func(g *InstanceGroup) { g.Placements = []Placement{{Zone: "ru-central1-b", SubnetID: "subnet-b"}} },
			wantErr: "zone, placements",
		},
		{
			name: "placements only",
			mutate: func(g *InstanceGroup) {
				g.Zone, g.SubnetID = "", ""
				g.Placements = []Placement{{Zone: "ru-central1-b", SubnetID: "subnet-b"}}
			},
		},
		{
			name: "placement without subnet",
			mutate: func(g *InstanceGroup) {
				g.Zone, g.SubnetID = "", ""
				g.Placements = []Placement{{Zone: "ru-central1-b"}}
			},
			wantErr: "placements[0].subnet_id",
		},
		{
			name:    "both user data",
			mutate:  func(g *InstanceGroup) { g.UserData, g.UserDataFile = "x", "/tmp/x" },
			wantErr: "user_data, user_data_file",
		},
		{
			name:    "reserved label",
			mutate:  func(g *InstanceGroup) { g.Labels = map[string]string{labelGroup: "x"} },
			wantErr: "reserved key in plugin config labels",
		},
		{
			name:    "reserved metadata",
			mutate:  func(g *InstanceGroup) { g.Metadata = map[string]string{metadataSSHKeys: "x"} },
			wantErr: "reserved key in plugin config metadata",
		},
		{name: "winrm without static credentials", settings: winrm, wantErr: "use_static_credentials"},
		{
			name: "winrm with static credentials",
			settings: provider.Settings{ConnectorConfig: provider.ConnectorConfig{
				Protocol:             provider.ProtocolWinRMHttps,
				UseStaticCredentials: true,
			}},
		},
		{
			name:     "unknown protocol",
			settings: provider.Settings{ConnectorConfig: provider.ConnectorConfig{Protocol: "telnet"}},
			wantErr:  "unsupported connector config protocol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := validGroup()
			g.settings = tt.settings
			if tt.mutate != nil {
				tt.mutate(g)
			}

			err := g.validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	g := &InstanceGroup{}
	err := g.validate()
	if err == nil {
		t.Fatal("validate() = nil for an empty config")
	}
	for _, want := range []string{"name", "folder_id", "zone", "subnet_id", "cores", "memory_gb", "disk_size_gb", "image_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validate() error does not mention %s: %v", want, err)
		}
	}
}

func TestPopulateDefaults(t *testing.T) {
	t.Setenv(keyFileEnv, "")

	g := validGroup()
	g.ImageID, g.ImageFamily = "", "ubuntu-2404-lts"
	if err := g.populate(); err != nil {
		t.Fatalf("populate() = %v", err)
	}

	if g.CoreFraction != 100 || g.DiskType != defaultDiskType || g.ImageFolderID != defaultImageFolderID {
		t.Fatalf("defaults: core_fraction=%d disk_type=%q image_folder_id=%q", g.CoreFraction, g.DiskType, g.ImageFolderID)
	}
	want := Placement{Zone: "ru-central1-a", SubnetID: "subnet-a", PlatformID: defaultPlatformID}
	if len(g.placements) != 1 || g.placements[0] != want {
		t.Fatalf("placements = %+v, want [%+v]", g.placements, want)
	}
}

func TestPopulateKeyFileFromEnv(t *testing.T) {
	t.Setenv(keyFileEnv, "/etc/gitlab-runner/yc-key.json")

	g := validGroup()
	g.ServiceAccountKeyFile = "/from/config.json"
	if err := g.populate(); err != nil {
		t.Fatalf("populate() = %v", err)
	}
	if g.ServiceAccountKeyFile != "/etc/gitlab-runner/yc-key.json" {
		t.Fatalf("ServiceAccountKeyFile = %q", g.ServiceAccountKeyFile)
	}
}

func TestPopulateUserDataFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "user-data.yml")
	if err := os.WriteFile(file, []byte("#cloud-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := validGroup()
	g.UserDataFile = file
	if err := g.populate(); err != nil {
		t.Fatalf("populate() = %v", err)
	}
	if g.UserData != "#cloud-config\n" {
		t.Fatalf("UserData = %q", g.UserData)
	}

	g = validGroup()
	g.UserDataFile = filepath.Join(t.TempDir(), "missing.yml")
	if err := g.populate(); err == nil {
		t.Fatal("populate() = nil for a missing user_data_file")
	}
}
