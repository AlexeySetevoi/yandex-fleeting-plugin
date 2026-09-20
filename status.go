package yandex

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/yandex-fleeting-plugin/internal/yandexapi"
)

// MapStatus переводит статус инстанса Compute API в состояние fleeting.
// Неизвестный статус — ok=false: состояние не угадываем, вызывающий пропускает.
func MapStatus(status string) (state provider.State, ok bool) {
	switch status {
	case yandexapi.StatusRunning, yandexapi.StatusUpdating:
		return provider.StateRunning, true
	case yandexapi.StatusProvisioning, yandexapi.StatusStarting, yandexapi.StatusRestarting:
		return provider.StateCreating, true
	case yandexapi.StatusStopping, yandexapi.StatusDeleting:
		return provider.StateDeleting, true
	case yandexapi.StatusStopped, yandexapi.StatusCrashed, yandexapi.StatusError:
		// Плагин машины не останавливает, только удаляет. STOPPED — это
		// вытесненная preemptible-машина (или остановленная руками): сама она
		// не вернётся, отдаём timeout, чтобы taskscaler её удалил.
		return provider.StateTimeout, true
	default:
		return "", false
	}
}
