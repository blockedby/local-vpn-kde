import type { Action } from "./bridge";

export const names: Partial<Record<Action, string>> = {
  "autostart/read": "Читаем настройки автозапуска",
  "autostart/set": "Сохраняем автозапуск",
  "backend/start": "Запускаем Docker",
  "network/reapply": "Применяем сетевые настройки VPN",
  start: "Подключаем VPN",
  disconnect: "Отключаем VPN",
  stop: "Останавливаем Docker",
  "toggle-mode": "Меняем режим",
  subscription: "Сохраняем подписку",
  "subscription/read": "Читаем подписку",
  diagnostics: "Проверяем компоненты",
  "servers/list": "Загружаем серверы",
  "servers/refresh": "Обновляем каталог",
  "servers/select": "Применяем сервер",
  "servers/ping": "Ping",
  "servers/speed": "Тест скорости",
  "servers/availability": "Доступность сайта",
};
