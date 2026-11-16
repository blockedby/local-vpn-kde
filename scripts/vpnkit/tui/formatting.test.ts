import { expect, test } from "bun:test";
import { pluralRu } from "./formatting";

test("Russian count forms handle teens and hundreds", () => {
  const forms = ["сервер", "сервера", "серверов"] as const;
  for (const [count, expected] of [[0, "серверов"], [1, "сервер"], [2, "сервера"], [5, "серверов"], [11, "серверов"], [21, "сервер"], [22, "сервера"], [25, "серверов"], [101, "сервер"], [111, "серверов"]] as const)
    expect(pluralRu(count, forms)).toBe(expected);
});
