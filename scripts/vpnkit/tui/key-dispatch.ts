import type { AppKeyEvent, AppState, Screen } from "./app-state";

export function dispatchKey(state: AppState, key: AppKeyEvent) {
    if (key.eventType === "release") return;
    const editor = state.screen === "subscription" || state.screen === "target" || state.screen === "profile-name" || state.screen === "profile-source";
    if (key.name === "escape") {
      key.preventDefault();
      state.open(
        state.screen.startsWith("profile-") || state.screen==="servers" && !!state.profiles
          ? "profiles"
          : editor && state.screen === "target"
          ? "servers"
          : state.screen === "confirm" && state.confirmation === "subscription"
            ? "subscription"
            : "home",
      );
      return;
    }
    if (key.ctrl && key.name === "c") {
      key.preventDefault();
      void state.close();
      return;
    }
    if (editor) {
      if (key.name === "return") {
        key.preventDefault();
        if(state.screen==="profile-name") {
          const value=state.editorValue.trim();if(!value || [...value].length>60){state.notify("Введите имя профиля: до 60 символов.",true);return}
          state.profileName=value;state.open("profile-source");return;
        }
        if(state.screen==="profile-source") {void state.addProfile(state.editorValue.trim());return}
        if (state.screen === "subscription") {
          const value = state.editorValue.trim();
          if (state.nativeInstalled()) {
            if (!value || !/^(https?:|vless:|hysteria2:|hy2:)\/\//.test(value)) {
              state.notify("Введите ссылку подписки.", true);
              return;
            }
            state.confirmSubscription(value);
          } else void state.perform("subscription", value);
        } else {
          try {
            const u = new URL(state.editorValue.trim());
            if (
              u.protocol !== "https:" ||
              u.username ||
              u.password ||
              u.href.length > 2048
            )
              throw new Error();
            state.target = u.href;
            state.servers.forEach((s) => (s.availability = "untested"));
            state.open("servers");
            state.notify("Адрес сайта сохранён.");
          } catch {
            state.notify("Нужен HTTPS-адрес без логина и пароля.", true);
          }
        }
      }
      return;
    }
    if (key.name === "q") {
      void state.close();
      return;
    }
    if (state.closing) return;
    if (key.name === "j") { state.open("tasks"); return; }
    if (key.name === "f" && state.recovery) {
      if (state.recovery.actions.includes("reapply")) void state.perform("network/reapply");
      else state.open("recovery");
      return;
    }
    if (key.name === "k") {
      state.cancel();
      return;
    }
    const screenKeys: Record<string, Screen> = {
      h: "home",
      v: "servers",
      c: state.nativeInstalled() && state.status?.subscription==="configured"?"profiles":"subscription",
      d: "diagnostics",
    };
    if (screenKeys[key.name]) {
      key.preventDefault();
      state.open(screenKeys[key.name]);
      return;
    }
    if (state.screen === "servers") {
      if (key.name === "a") {
        void state.runBatch("ping");
        return;
      }
      if (key.name === "tab" && state.servers.length) {
        state.serverMenu = !state.serverMenu;
        state.paint();
        return;
      }
      if (key.name === "o") {
        const sorts = ["name", "ping", "speed", "availability"] as const;
        state.frozenOrder = undefined;
        state.sort = sorts[(sorts.indexOf(state.sort) + 1) % sorts.length];
        state.viewStart = 0;
        if (state.batch) state.frozenOrder = state.sortedServers().map(s => s.server_id);
        state.paint();
        return;
      }
      if (
        !state.serverMenu &&
        state.servers.length &&
        (key.name === "up" ||
          key.name === "down" ||
          key.name === "pageup" ||
          key.name === "pagedown")
      ) {
        const list = state.sortedServers();
        const at = Math.max(
          0,
          list.findIndex((s) => s.server_id === state.serverID),
        );
        const step = key.name.startsWith("page") ? state.rowCount() : 1;
        state.serverID =
          list[
            Math.max(
              0,
              Math.min(
                list.length - 1,
                at + (["up", "pageup"].includes(key.name) ? -step : step),
              ),
            )
          ]?.server_id;
        state.paint();
        return;
      }
      if (key.name === "return" && !state.serverMenu && state.servers.length) {
        if (state.catalogStale) {
          state.notify("Подписка изменена. Обновите список серверов.", true);
          return;
        }
        const s = state.servers.find((s) => s.server_id === state.serverID);
        if (s) void state.perform("servers/select", s.server_id);
        return;
      }
    }
    const items = state.items();
    if (key.name === "up" || (key.name === "tab" && key.shift))
      state.selected =
        (state.selected + items.length - 1) % Math.max(1, items.length);
    else if (key.name === "down" || key.name === "tab")
      state.selected = (state.selected + 1) % Math.max(1, items.length);
    else if (key.name === "return") items[state.selected]?.run();
    else if (key.name === "u" && !items.some(item => item.key === "u"))
      void state.perform(
        state.screen === "diagnostics" ? "diagnostics" : "status",
      );
    else {
      const item = items.find((i) => i.key === key.name);
      if (item) {
        key.preventDefault();
        item.run();
      }
    }
    state.paint();
}
