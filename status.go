package yandex

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/yandex-fleeting-plugin/internal/yandexapi"
)

// MapStatus переводит статус инстанса Compute API в состояние fleeting.
// Неизвестный статус — ok=false: состояние не угадываем, решает вызывающий.
//
// StateTimeout не отдаём никогда: для taskscaler это «инстанса больше нет», он
// забывает его без Decrease, и ВМ остаётся в каталоге навсегда.
func MapStatus(status string) (state provider.State, ok bool) {
	switch status {
	case yandexapi.StatusRunning, yandexapi.StatusUpdating:
		return provider.StateRunning, true
	case yandexapi.StatusProvisioning, yandexapi.StatusStarting, yandexapi.StatusRestarting:
		return provider.StateCreating, true
	case yandexapi.StatusStopping, yandexapi.StatusDeleting:
		return provider.StateDeleting, true
	}
	if Broken(status) {
		// Update такие ВМ удаляет сам, см. Broken
		return provider.StateDeleting, true
	}
	return "", false
}

// Broken — ВМ сама не вернётся, и удалить её должен плагин. Плагин машины не
// останавливает, только удаляет: STOPPED — вытесненная preemptible-машина (или
// остановленная руками), CRASHED и ERROR — сломанная.
func Broken(status string) bool {
	switch status {
	case yandexapi.StatusStopped, yandexapi.StatusCrashed, yandexapi.StatusError:
		return true
	}
	return false
}
