# Проверка Go-порта

Локально выполнено 17 октября 2026 года:

- `go test -race ./...` — успешно, включая сценарии lost-response/restart,
  позднего capture, незавершённой пагинации, 401/429/5xx, ротации refresh token,
  outbox, полного/частичного/последовательных возвратов и операторского восстановления.
- SMTP проверен на локальном TLS-сервере: корректная передача письма,
  отказ от недоверенного сертификата, успешный DATA ACK при потерянном QUIT ACK.
- SQLite: запрет второго writer, согласованный backup, права 0600,
  отказ от symlink и от использования Python-state как Go-state.
- `go vet ./...` — успешно.
- `go build -trimpath -o bin/receipts ./cmd/receipts` и `receipts version` — успешно.
- `docker compose config --no-env-resolution --quiet` — успешно.
- `docker build -t fxtunnel-receipts:go-port-review .` — успешно.
- `python3 scripts/smoke-go-container.py --image fxtunnel-receipts:go-port-review` — успешно:
  `/readyz` отвечает, процесс non-root, rootfs read-only, backup читается,
  второй writer заблокирован. Внешняя сеть контейнера выключена, состояние
  временное, реальных ключей нет, записей доходов нет. Контейнер удалён после проверки.
- `git diff --check` — успешно.

CI повторяет race/vet/build и контейнерный smoke. SQLite C compiler выдаёт
предупреждение в исходниках зависимости go-sqlite3; сборка и проверки завершаются успешно.

Не проверялись реальная сессия пользователя в ФНС, настоящий магазин ЮKassa,
доставка через production SMTP и боевые возвраты. Все сетевые контракты в тестах
обслуживались локальными серверами. Production не развёртывался.
