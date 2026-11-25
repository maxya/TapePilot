export type Session = { authenticated: boolean; csrf_token: string };
export type SystemInfo = {
  mode: string;
  restore_roots: string[];
  legacy_roots: string[];
};
export type Job = {
  id: string;
  kind: string;
  state: string;
  phase: string;
  volume_id: string;
  created_at: string;
  completed_at?: string;
  error_code?: string;
};
export type Volume = {
  id: string;
  kind: string;
  label: string;
  location: string;
  barcode?: string;
  generation: string;
  state: string;
  header_sha256?: string;
  created_at: string;
};
export type Archive = {
  id: string;
  volume_id: string;
  job_id: string;
  sequence: number;
  data_file_index: number;
  commit_file_index?: number | null;
  source_label: string;
  created_at: string;
  stream_bytes: number;
  sha256: string;
  integrity: string;
  integrity_basis: string;
  verified_at?: string | null;
  restore_eligible: boolean;
};
export type Member = {
  id: number;
  path: string;
  raw_path: string;
  parent_path: string;
  type: string;
  size: number;
  mtime: string;
  mode: number;
  link_target: string;
  archive_id?: string;
  volume_id?: string;
  volume_label?: string;
  integrity?: string;
  integrity_basis?: string;
};
export type Profile = {
  id: string;
  name: string;
  source_id: string;
  relative_selection: string;
  exclusions: string[];
  target_volume_id: string;
  enabled: boolean;
  revision: number;
  updated_at: string;
};
export type Schedule = {
  operation: "BACKUP" | "VERIFY";
  id: string;
  profile_id: string;
  cadence: "daily" | "weekly";
  timezone: string;
  local_time: string;
  weekday: number;
  enabled: boolean;
  revision: number;
  updated_at: string;
};
export type Notification = {
  id: number;
  job_id: string;
  code: string;
  message: string;
  created_at: string;
  read_at: string | null;
};
export type Drive = {
  id: string;
  mode: string;
  status?: string;
  cartridge_id?: string;
  generation?: string;
  capacity_bytes?: number;
  used_bytes?: number;
  write_protected?: boolean;
  uncertain?: boolean;
  lease_busy?: boolean;
};
export type BackupPlan = {
  id: string;
  revision: number;
  entries: number;
  logical_bytes: number;
  estimated_bytes: number;
  expires_at: string;
};
export type RestoreOptions = {
  archive_id: string;
  member_ids: number[];
  all: boolean;
  restore_root_id: string;
  destination_slug: string;
  legacy_source_id: string;
  legacy_profile: string;
  legacy_confirmation: boolean;
};
export type RestorePlan = {
  id: string;
  archive_id: string;
  volume_id: string;
  restore_root_id: string;
  destination_slug: string;
  requested_members: number;
  selected_members: number;
  expanded_members: number;
  output_bytes: number;
  spool_bytes: number;
  members: { id: number; path: string; type: string; size: number }[];
  truncated: boolean;
};
export type Progress = {
  phase: string;
  accepted_bytes: number;
  readback_bytes: number;
  elapsed_seconds: number;
  speed_mib_per_second: number;
  estimated_percent: number | null;
  eta_seconds: number | null;
};
export type JobEvent = {
  id: number;
  job_id: string;
  timestamp: string;
  code: string;
  message: string;
};

export class APIError extends Error {
  status: number;
  code: string;
  constructor(message: string, status = 0, code = "CONNECTION_FAILED") {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.code = code;
  }
}

export class API {
  private csrf = "";
  private request: typeof fetch;
  constructor(request: typeof fetch = fetch) {
    this.request = request;
  }
  session(token: string): void {
    this.csrf = token;
  }
  get<T>(path: string): Promise<T> {
    return this.send<T>("GET", path).then((result) => result.value);
  }
  mutate<T>(
    path: string,
    body: unknown,
    key?: string,
    method = "POST",
  ): Promise<T> {
    return this.send<T>(method, path, body, key).then((result) => result.value);
  }
  page<T>(path: string): Promise<{ items: T[]; cursor: string }> {
    return this.send<T[]>("GET", path).then((result) => ({
      items: result.value,
      cursor: result.cursor,
    }));
  }
  private async send<T>(
    method: string,
    path: string,
    body?: unknown,
    key?: string,
  ): Promise<{ value: T; cursor: string }> {
    if (!path.startsWith("/") || path.startsWith("//"))
      throw new APIError("Invalid API route.");
    if (method !== "GET" && !this.csrf)
      throw new APIError(
        "Refresh your session before continuing.",
        401,
        "SESSION_REQUIRED",
      );
    const headers: Record<string, string> = { Accept: "application/json" };
    if (method !== "GET") {
      headers["Content-Type"] = "application/json";
      headers["X-CSRF-Token"] = this.csrf;
      if (key) headers["Idempotency-Key"] = key;
    }
    let response: Response;
    try {
      response = await this.request("/api/v1" + path, {
        method,
        headers,
        credentials: "same-origin",
        cache: "no-store",
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(20000),
      });
    } catch {
      throw new APIError(
        "The server could not be reached. Check the connection and try again.",
      );
    }
    let result: unknown;
    if (response.status !== 204) {
      try {
        result = await response.json();
      } catch {
        throw new APIError(
          "The server returned an unreadable response.",
          response.status,
        );
      }
    }
    if (!response.ok) {
      const envelope = result as
        { error?: { code?: string; message?: string } } | undefined;
      throw new APIError(
        envelope?.error?.message || "The request could not be completed.",
        response.status,
        envelope?.error?.code || "REQUEST_FAILED",
      );
    }
    return {
      value: result as T,
      cursor: response.headers.get("X-Next-Cursor") || "",
    };
  }
}

export function newRunKey(): string {
  return crypto.randomUUID();
}
export function bytes(value: number | null | undefined): string {
  if (
    value === null ||
    value === undefined ||
    !Number.isFinite(value) ||
    value < 0
  )
    return "Unknown";
  if (value < 1024) return value + " B";
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  let n = value / 1024;
  let unit = 0;
  while (n >= 1024 && unit < units.length - 1) {
    n /= 1024;
    unit++;
  }
  return (
    n.toLocaleString(undefined, { maximumFractionDigits: 1 }) +
    " " +
    units[unit]
  );
}
export function instant(value?: string | null): string {
  if (!value) return "Unknown";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Unknown" : date.toLocaleString();
}
export function label(value: string): string {
  return value.toLowerCase().replaceAll("_", " ");
}
export function terminal(state: string): boolean {
  return ["SUCCEEDED", "FAILED", "CANCELLED", "RECOVERY_REQUIRED"].includes(
    state,
  );
}

export function failureMessage(code: string, fallback?: string): string {
  const messages: Record<string, string> = {
    UNSAFE_ARCHIVE:
      "This archive contains unsafe or unsupported entries. Restore is blocked.",
    INSUFFICIENT_SPACE:
      "There is insufficient space for the verified spool and restored files. Free space in the configured roots before retrying.",
    INSUFFICIENT_RESTORE_SPACE:
      "There is insufficient space for the verified spool and restored files. Free space in the configured roots before retrying.",
    RESTORE_DESTINATION_UNAVAILABLE:
      "The restore destination already exists or is unavailable. Choose a fresh destination name.",
    DESTINATION_EXISTS_OR_UNSAFE:
      "The restore destination already exists or is unavailable. Choose a fresh destination name.",
    SOURCE_CHANGED:
      "The source changed after preview. Create a fresh backup preview before running.",
    MEDIA_MISMATCH:
      "The selected tape is not loaded. Insert the required tape and recheck the job.",
    WRITE_NOT_ARMED:
      "Tape write enrollment is missing or revoked. Review enrollment with the local CLI.",
    VERIFICATION_FAILED:
      "Readback did not match the expected archive. Keep the tape read-only while investigating.",
    RECOVERY_REQUIRED:
      "The operation is uncertain. Review its recorded events and keep the tape read-only while investigating.",
    CAPACITY_BUDGET_EXCEEDED:
      "The archive exceeds the configured capacity budget. Review the source selection and target tape.",
  };
  return (
    messages[code] ||
    fallback ||
    label(code) + ". Review the recorded events for this job."
  );
}

export type DoctorCheck = {
  id: string;
  status: "ok" | "warning" | "error";
  message: string;
  version?: string;
  available_bytes?: number;
  required_bytes?: number;
  identity?: string;
};
export type DoctorReport = {
  mode: string;
  checked_at: string;
  ready: boolean;
  physical_read_configured: boolean;
  checks: DoctorCheck[];
};

// Restore eligibility starts with current byte-integrity evidence. The server's
// preview still validates the complete archive graph and private spool policy.
export function restoreIntegrityUsable(
  archive: Pick<Archive, "integrity"> | undefined,
): boolean {
  return archive?.integrity === "VERIFIED" || archive?.integrity === "OBSERVED";
}
export function verifiedArchiveCount(
  archives: Pick<Archive, "integrity">[],
): number {
  return archives.filter((archive) => archive.integrity === "VERIFIED").length;
}
export function historicalVerification(
  archive: Pick<Archive, "integrity" | "verified_at" | "commit_file_index">,
): string | undefined {
  if (
    archive.integrity === "OBSERVED" ||
    archive.commit_file_index === null ||
    archive.commit_file_index === undefined
  )
    return undefined;
  return archive.verified_at || undefined;
}
