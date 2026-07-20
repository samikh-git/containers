// Scratch harness for eyeballing Streamdown's rendering in a browser without
// needing a live router/agent backend. Not part of the shipped app.
import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { Streamdown } from "streamdown";
import { code } from "@streamdown/code";

const sample = `# Deploying the gateway

Here's what I changed to fix the **health check** flake:

1. Added a retry with backoff around the readiness probe
2. Bumped the timeout from \`2s\` to \`5s\`
3. Logged the failing status code

\`\`\`go
func waitReady(ctx context.Context, url string) error {
	for i := 0; i < 5; i++ {
		if err := ping(url); err == nil {
			return nil
		}
		time.Sleep(time.Second * time.Duration(i))
	}
	return fmt.Errorf("gateway not ready")
}
\`\`\`

Some notes:

> The old probe hit \`/healthz\` before the router had registered routes, so it always 404'd on cold start.

| File | Change |
| --- | --- |
| \`router/serve.go\` | added retry loop |
| \`gateway.json\` | bumped timeout |

Links: see [the PR](https://example.com) for the full diff.
`;

function Preview() {
  const [text, setText] = useState(sample);
  return (
    <div style={{ display: "flex", height: "100%" }}>
      <textarea
        value={text}
        onChange={(e) => setText(e.target.value)}
        style={{
          width: "38%",
          minWidth: 320,
          background: "var(--panel)",
          color: "var(--fg)",
          border: "none",
          borderRight: "1px solid var(--border)",
          padding: 16,
          fontFamily: "var(--mono)",
          fontSize: 13,
          resize: "none",
        }}
      />
      <div style={{ flex: 1, overflow: "auto", background: "var(--bg)", padding: "40px 24px" }}>
        <div className="thread-inner" style={{ maxWidth: 760 }}>
          <div className="msg assistant">
            <Streamdown className="md" plugins={{ code }} shikiTheme={["github-light", "github-dark"]}>
              {text}
            </Streamdown>
          </div>
        </div>
      </div>
    </div>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Preview />
  </StrictMode>,
);
