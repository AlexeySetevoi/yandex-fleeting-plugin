// Command smoketest гоняет InstanceGroup по настоящему Yandex Cloud:
// Init -> Increase(1) -> опрос Update до Running -> пауза -> ConnectInfo ->
// реальный вход по SSH/WinRM -> Decrease -> Shutdown. Проверяет то, что
// юнит-тестами не покрыть: слой над go-sdk, реальные статусы инстансов и то,
// что данные из ConnectInfo действительно пускают на машину. Подключается тем
// же пакетом fleeting/connector, что и сам GitLab Runner.
//
// Запуск:
//
//	YC_SERVICE_ACCOUNT_KEY_FILE=key.json go run ./cmd/smoketest \
//	    -folder-id=b1g... -zone=ru-central1-a -subnet-id=e9b... \
//	    -image-family=ubuntu-2404-lts -nat
//
// Проверка fallback по размещениям — первым идёт вариант заведомо без ресурсов:
//
//	... -placements=<zone>:<subnet_id>:<platform_id>,ru-central1-a:e9b...:standard-v3
//
// Windows/WinRM (пароль уже задан в образе или через -user-data-file):
//
//	... -image-id=fd8... -protocol=winrm -username=Administrator -password=...
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/connector"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	yandex "github.com/AlexeySetevoi/yandex-fleeting-plugin"
)

const accessCheckAttemptTimeout = 20 * time.Second

func main() {
	name := flag.String("name", "smoketest", "instance group name / instance name prefix")
	folderID := flag.String("folder-id", "", "Yandex Cloud folder id (required)")
	keyFile := flag.String("key-file", "", "service account authorized key file (or YC_SERVICE_ACCOUNT_KEY_FILE)")
	zone := flag.String("zone", "", "availability zone, e.g. ru-central1-a")
	subnetID := flag.String("subnet-id", "", "subnet id in that zone")
	platformID := flag.String("platform-id", "", "platform id (default standard-v3)")
	placements := flag.String("placements", "", "comma separated zone:subnet_id[:platform_id] fallback list, instead of -zone/-subnet-id/-platform-id")
	cores := flag.Int("cores", 2, "vCPU count")
	memoryGB := flag.Float64("memory-gb", 2, "RAM, GB")
	coreFraction := flag.Int("core-fraction", 20, "guaranteed vCPU share, %")
	imageID := flag.String("image-id", "", "image id")
	imageFamily := flag.String("image-family", "", "image family (alternative to -image-id), e.g. ubuntu-2404-lts")
	imageFolderID := flag.String("image-folder-id", "", "folder to look the image family up in (default standard-images)")
	diskSizeGB := flag.Int("disk-size-gb", 20, "boot disk size, GB")
	nat := flag.Bool("nat", false, "give the instance a public address")
	securityGroupIDs := flag.String("security-group-ids", "", "comma separated security group ids")
	preemptible := flag.Bool("preemptible", false, "create a preemptible instance")
	userDataFile := flag.String("user-data-file", "", "path to cloud-init / #ps1 user-data")
	protocol := flag.String("protocol", "ssh", "connector protocol: ssh, winrm or winrm+https")
	username := flag.String("username", "", "connector username (default ubuntu / Administrator)")
	password := flag.String("password", "", "static connector password (required for winrm)")
	useExternalAddr := flag.Bool("use-external-addr", true, "connect to the public address; set false when the subnet is reachable directly")
	pollInterval := flag.Duration("poll-interval", 10*time.Second, "how often to call Update while waiting for the instance")
	readyTimeout := flag.Duration("ready-timeout", 5*time.Minute, "how long to wait for state Running")
	settle := flag.Duration("settle", 30*time.Second, "pause between Running and the access check")
	accessTimeout := flag.Duration("access-timeout", 3*time.Minute, "how long to keep retrying the access check")
	accessRetryInterval := flag.Duration("access-retry-interval", 10*time.Second, "delay between access check attempts")
	checkCmd := flag.String("check-cmd", "", "override the access check command")
	keep := flag.Bool("keep", false, "skip Decrease/Shutdown, leave the instance for manual inspection")
	flag.Parse()

	if *folderID == "" {
		fmt.Fprintln(os.Stderr, "usage: smoketest -folder-id=<id> (-zone=<zone> -subnet-id=<id> | -placements=...) (-image-id=<id> | -image-family=<family>) ...")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logger := hclog.New(&hclog.LoggerOptions{
		Name:  "smoketest",
		Level: hclog.Debug,
	})

	g := &yandex.InstanceGroup{
		Name:                  *name,
		FolderID:              *folderID,
		ServiceAccountKeyFile: *keyFile,
		Zone:                  *zone,
		SubnetID:              *subnetID,
		PlatformID:            *platformID,
		Placements:            parsePlacements(*placements),
		Cores:                 *cores,
		MemoryGB:              *memoryGB,
		CoreFraction:          *coreFraction,
		ImageID:               *imageID,
		ImageFamily:           *imageFamily,
		ImageFolderID:         *imageFolderID,
		DiskSizeGB:            *diskSizeGB,
		NAT:                   *nat,
		Preemptible:           *preemptible,
		UserDataFile:          *userDataFile,
	}
	if *securityGroupIDs != "" {
		g.SecurityGroupIDs = strings.Split(*securityGroupIDs, ",")
	}

	settings := provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{
			Protocol:             provider.Protocol(*protocol),
			Username:             *username,
			Password:             *password,
			UseStaticCredentials: *password != "",
			Timeout:              15 * time.Second,
			Keepalive:            10 * time.Second,
		},
	}

	info, err := g.Init(ctx, logger, settings)
	if err != nil {
		log.Fatalf("Init: %v", err)
	}
	logger.Info("initialized", "provider_id", info.ID, "max_size", info.MaxSize)

	succeeded, err := g.Increase(ctx, 1)
	if err != nil {
		logger.Error("increase reported an error", "error", err)
	}
	if succeeded != 1 {
		log.Fatalf("Increase: succeeded=%d, want 1", succeeded)
	}

	// дальше инстанс существует и должен быть убран при любом исходе
	var instanceID string
	defer func() {
		if instanceID == "" {
			return
		}
		if *keep {
			logger.Info("keeping instance alive (-keep set); clean it up manually in the console", "id", instanceID)
			return
		}

		deleted, err := g.Decrease(context.Background(), []string{instanceID})
		if err != nil {
			logger.Error("decrease reported an error", "error", err)
		}
		logger.Info("decreased", "deleted", deleted)

		if err := g.Shutdown(context.Background()); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
	}()

	instanceID, err = waitUntilRunning(ctx, logger, g, *pollInterval, *readyTimeout)
	if err != nil {
		logger.Error("waiting for instance failed", "error", err)
		return
	}

	logger.Info("instance is running, letting it settle before checking access", "duration", settle.String())
	select {
	case <-ctx.Done():
		return
	case <-time.After(*settle):
	}

	connectInfo, err := g.ConnectInfo(ctx, instanceID)
	if err != nil {
		logger.Error("connect info failed", "error", err)
		return
	}
	logger.Info("connect info",
		"external_addr", connectInfo.ExternalAddr,
		"internal_addr", connectInfo.InternalAddr,
		"username", connectInfo.Username,
		"protocol", connectInfo.Protocol,
		"has_key", len(connectInfo.Key) > 0,
		"has_password", connectInfo.Password != "",
	)

	if err := checkAccess(ctx, logger, connectInfo, *accessTimeout, *accessRetryInterval, *checkCmd, *useExternalAddr); err != nil {
		logger.Error("ACCESS CHECK FAILED", "error", err)
	} else {
		logger.Info("ACCESS CHECK PASSED")
	}
}

func parsePlacements(value string) []yandex.Placement {
	if value == "" {
		return nil
	}

	var result []yandex.Placement
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(item, ":")
		if len(parts) < 2 || len(parts) > 3 {
			log.Fatalf("invalid -placements item %q, want zone:subnet_id[:platform_id]", item)
		}

		p := yandex.Placement{Zone: parts[0], SubnetID: parts[1]}
		if len(parts) == 3 {
			p.PlatformID = parts[2]
		}
		result = append(result, p)
	}
	return result
}

// waitUntilRunning опрашивает Update, пока инстанс не станет Running, и пишет
// каждое увиденное состояние. Последний увиденный id возвращает и при
// таймауте — чтобы вызывающий смог убрать машину, так и не дошедшую до Running.
func waitUntilRunning(ctx context.Context, logger hclog.Logger, g *yandex.InstanceGroup, pollInterval, readyTimeout time.Duration) (string, error) {
	deadline := time.Now().Add(readyTimeout)

	var lastSeen string

	for {
		var found string
		var state provider.State

		if err := g.Update(ctx, func(instance string, s provider.State) {
			found = instance
			state = s
		}); err != nil {
			logger.Error("update failed", "error", err)
		}

		if found != "" {
			lastSeen = found
			logger.Info("observed instance state", "id", found, "state", state)
			if state == provider.StateRunning {
				return found, nil
			}
		}

		if time.Now().After(deadline) {
			return lastSeen, fmt.Errorf("timed out after %s waiting for the instance to become ready", readyTimeout)
		}

		select {
		case <-ctx.Done():
			return lastSeen, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// checkAccess реально подключается к инстансу и выполняет простую команду,
// повторяя попытки до успеха или таймаута: машина в RUNNING ещё какое-то время
// грузится и применяет cloud-init.
func checkAccess(ctx context.Context, logger hclog.Logger, info provider.ConnectInfo, timeout, interval time.Duration, command string, useExternalAddr bool) error {
	if command == "" {
		command = accessCheckCommand(info.Protocol)
	}

	deadline := time.Now().Add(timeout)

	for {
		var stdout, stderr bytes.Buffer

		attemptCtx, cancel := context.WithTimeout(ctx, accessCheckAttemptTimeout)
		err := connector.Run(attemptCtx, info, connector.ConnectorOptions{
			RunOptions: connector.RunOptions{
				Command: command,
				Stdout:  &stdout,
				Stderr:  &stderr,
			},
			DialOptions: connector.DialOptions{UseExternalAddr: useExternalAddr},
		})
		cancel()

		if err == nil {
			logger.Info("access check succeeded", "stdout", strings.TrimSpace(stdout.String()))
			return nil
		}

		logger.Debug("access check attempt failed, retrying", "error", err, "stderr", strings.TrimSpace(stderr.String()))

		if time.Now().After(deadline) {
			return fmt.Errorf("giving up after %s: %w", timeout, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func accessCheckCommand(protocol provider.Protocol) string {
	switch protocol {
	case provider.ProtocolWinRM, provider.ProtocolWinRMHttps:
		return "whoami"
	default:
		return "echo smoketest-ok && uname -a"
	}
}
