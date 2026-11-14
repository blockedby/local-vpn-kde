import type { CliRenderer } from "@opentui/core";

// Register immediately: output can fail before the application asks to exit.
export function observeRendererClose(renderer: CliRenderer): Promise<void> {
  return renderer.closed.catch(error => {
    process.exitCode = 1;
    console.error("Не удалось восстановить терминал:", error);
  });
}
