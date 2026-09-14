# Local VPN KDE

Локальный VPN для KDE: шлюз в Docker, подключение через NetworkManager/OpenVPN и терминальный интерфейс OpenTUI. Проверяет ping и доступность сайта по 5 серверов одновременно; скорость — отдельным тестом.

**Требования:** Linux с KDE, Docker Compose, NetworkManager с поддержкой OpenVPN, Python 3, Bun 1.3+ и `sudo` для настройки маршрутов.

## Первый запуск

```bash
cd ~/code/tools/local-vpn-kde
(cd scripts/vpnkit/tui && bun install --frozen-lockfile)
install -d -m 700 secrets secrets/vpnkit-local secrets/vpnkit-local/vibe-vpn
./run.sh
```

В меню «Подписка» укажи URL и выйди. Затем:

```bash
./install.sh
./run.sh
```

Запускай от обычного пользователя, без `sudo`. Установщик сам запросит необходимые права, соберёт и запустит Docker-шлюз, настроит маршруты и импортирует профиль KDE. VPN автоматически не подключается.

Для дальнейшей работы достаточно `./run.sh`. Подписка, ключи и журналы хранятся в `secrets/` и не попадают в Git.
