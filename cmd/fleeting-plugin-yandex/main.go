package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	yandex "github.com/AlexeySetevoi/yandex-fleeting-plugin"
)

func main() {
	plugin.Main(&yandex.InstanceGroup{}, yandex.Version)
}
