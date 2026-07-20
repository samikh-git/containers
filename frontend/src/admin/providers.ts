/** Known provider hosts → display name + key placeholder. */
export const PROVIDERS: [string, string, string][] = [
  ["api.anthropic.com", "Anthropic", "sk-ant-…"],
  ["api.openai.com", "OpenAI", "sk-…"],
  ["generativelanguage.googleapis.com", "Google Gemini", "AIza…"],
  ["api.x.ai", "xAI (Grok)", "xai-…"],
  ["api.mistral.ai", "Mistral", ""],
  ["api.deepseek.com", "DeepSeek", "sk-…"],
  ["api.groq.com", "Groq", "gsk_…"],
  ["openrouter.ai", "OpenRouter", "sk-or-…"],
  ["api.together.xyz", "Together AI", ""],
  ["api.cohere.com", "Cohere", ""],
];

export function providerOf(
  route: string,
  info: { base_url?: string },
): { name: string; hint: string } {
  let host = "";
  try {
    host = new URL(info.base_url ?? "").hostname;
  } catch {
    /* ignore */
  }
  for (const [h, name, hint] of PROVIDERS) {
    if (host === h) return { name, hint };
  }
  if (
    host === "127.0.0.1" ||
    host === "localhost" ||
    host.endsWith(".local") ||
    host.endsWith(".internal")
  ) {
    return { name: `Local (${host})`, hint: "usually none" };
  }
  return { name: route, hint: "" };
}
