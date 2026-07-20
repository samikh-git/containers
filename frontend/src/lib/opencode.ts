// Per-workspace OpenCode SDK client, pointed at the router's reverse proxy.
// All calls ride the same origin and carry the router API token; the router
// strips that token before forwarding into the sandbox.
import { createOpencodeClient, type OpencodeClient } from "@opencode-ai/sdk/client";
import { authHeaders } from "./router";

export type { OpencodeClient };

export function opencodeClient(workspaceID: string): OpencodeClient {
  return createOpencodeClient({
    baseUrl: `${location.origin}/api/workspaces/${workspaceID}/opencode`,
    headers: authHeaders(),
  });
}
