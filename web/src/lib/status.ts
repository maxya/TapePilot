export async function connected(request: typeof fetch): Promise<boolean> {
  try {
    const response = await request("/api/v1/health", {
      cache: "no-store",
      signal: AbortSignal.timeout(4000),
    });
    if (!response.ok) return false;
    const body = await response.json();
    return body?.status === "ok" && body?.mode === "simulator";
  } catch {
    return false;
  }
}
