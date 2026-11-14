import { expect, test } from "bun:test";
import { serverViewport, sortedServers } from "./catalog-order";
import { rows } from "./app-test-fixtures";

test("catalog order is stable by identity and freezes while measurements change", () => {
  const servers = [
    { ...rows[0], display_name: "Node 10", latency_ms: 10, download_mbps: 20, availability: "ready" },
    { ...rows[1], display_name: "Node 2", latency_ms: 20, download_mbps: 30, availability: "failed" },
  ];
  const ids = (sort: Parameters<typeof sortedServers>[1]) => sortedServers(servers, sort).map(s => s.server_id);
  expect(ids("name")).toEqual([rows[1].server_id, rows[0].server_id]);
  expect(ids("ping")).toEqual([rows[0].server_id, rows[1].server_id]);
  expect(ids("speed")).toEqual([rows[1].server_id, rows[0].server_id]);
  expect(ids("availability")).toEqual([rows[0].server_id, rows[1].server_id]);
  expect(sortedServers(servers, "speed", [rows[0].server_id, rows[1].server_id]).map(s => s.server_id)).toEqual(ids("ping"));
  expect(servers.map(s => s.server_id)).toEqual(ids("ping"));
});

test("catalog viewport keeps selection visible and clamps after removal", () => {
  const servers = Array.from({ length: 10 }, (_, i) => ({ ...rows[0], server_id: String(i) }));
  expect(serverViewport(servers, "7", 1, 3).start).toBe(5);
  expect(serverViewport(servers, "2", 5, 3).start).toBe(2);
  const viewport = serverViewport(servers.slice(0, 4), "3", 7, 3);
  expect(viewport.start).toBe(1);
  expect(viewport.servers.map(s => s.server_id)).toEqual(["1", "2", "3"]);
});
