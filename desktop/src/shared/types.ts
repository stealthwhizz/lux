// Shapes of Lux API (v1) responses used by the desktop app.

export type TaskState =
  | "queued" | "starting" | "running" | "waiting_approval" | "cancelling"
  | "succeeded" | "failed" | "cancelled" | "timed_out";

export const TERMINAL_STATES: TaskState[] = ["succeeded", "failed", "cancelled", "timed_out"];
export const isTerminal = (s: TaskState) => TERMINAL_STATES.includes(s);

export interface Profile {
  name: string;
  url: string;
  local: boolean;
  workspace: string;
  signedIn: boolean;
  /** Where its token is kept: the CLI's credentials file, the app's encrypted store, or memory only. */
  tokenStorage: "cli" | "encrypted" | "session" | "none";
}

export interface ProfilesState {
  current: string;
  profiles: Profile[];
  /** False when the OS offers no secure store; tokens are then kept for this session only. */
  secureStorage: boolean;
}

export interface AppInfo {
  version: string;
  platform: string;
  secureStorage: boolean;
  storageBackend: string;
}

export interface ServerStatus {
  version: string;
  api_version: string;
  mode: "local" | "server";
  // Server mode leaves these out until the caller is signed in.
  schema_version?: number;
  bootstrapped: boolean;
  components?: Record<string, string>;
  executors?: { name: string; isolation: string; available: boolean; note?: string }[];
  model_providers?: { name: string; base_url: string; has_key: boolean; note?: string }[];
}

export interface Me {
  user_id: string;
  username: string;
  workspaces: { id: string; slug: string; name: string; role: string }[];
}

export interface Issue { severity: string; code: string; path?: string; message: string }

export interface Report {
  valid: boolean;
  errors: Issue[];
  warnings: Issue[];
  code_execution?: string[];
  models?: { provider: string; model: string }[];
}

export interface Revision {
  id: string;
  commit_sha: string;
  valid: boolean;
  manifest: Record<string, unknown>;
  validation: Report;
  created_at: string;
}

export interface Agent {
  id: string;
  slug: string;
  name: string;
  description: string;
  source_kind: "git" | "local" | "managed";
  source_url: string;
  source_ref: string;
  runtime: string;
  executor: string;
  disabled: boolean;
  revision?: Revision | null;
  task_counts?: Record<string, number>;
  last_task_at?: string | null;
}

export interface Task {
  id: string;
  repository_id?: string;
  agent_id: string;
  agent_name: string;
  commit_sha: string;
  conversation_id?: string;
  prompt: string;
  state: TaskState;
  state_reason: string;
  result_summary: string;
  executor: string;
  isolation: string;
  attempt: number;
  event_seq: number;
  created_at: string;
  started_at?: string | null;
  finished_at?: string | null;
}

export interface TaskEvent {
  seq: number;
  type: string;
  payload: Record<string, any>;
  created_at: string;
}

export interface Approval {
  id: string;
  task_id: string;
  agent_id?: string;
  agent_name: string;
  kind: string;
  summary: string;
  details: Record<string, any>;
  rule: string;
  state: "pending" | "approved" | "rejected" | "expired" | "cancelled";
  decided_by?: string;
  decision_note?: string;
  created_at: string;
  expires_at?: string | null;
}

export interface Conversation {
  id: string;
  agent_id: string;
  agent_name: string;
  title: string;
  active_task_id?: string;
  last_message?: Message;
  updated_at: string;
}

export interface Attachment { path: string; name: string; size: number; text_path?: string }

export interface Message {
  id: string;
  attachments?: Attachment[];
  role: "user" | "assistant" | "system";
  content: string;
  task_id?: string;
  created_at: string;
}

export interface LocalServerStatus {
  available: boolean; // the lux CLI was found
  running: boolean;
  url?: string;
  pid?: number;
  message?: string;
}

export interface Repository {
  id: string;
  name: string;
  source_kind: "git" | "local";
  source_url: string;
  default_branch: string;
  test_command: string;
  credential?: string;
}

export interface FileChange { path: string; added: number; deleted: number; binary?: boolean }

export interface Change {
  task_id: string;
  target: "repository" | "memory";
  files: FileChange[];
  patch_bytes: number;
  patch_available: boolean;
  state: "proposed" | "publishing" | "published" | "discarded";
  branch?: string;
  commit_sha?: string;
  pull_request_url?: string;
  last_error?: string;
  created_at: string;
  task_prompt?: string;
  agent_name?: string;
}

export interface WorkspaceFile { path: string; size: number; modified: string }

export type FilePreview = { kind: "text"; text: string } | { kind: "image"; dataUrl: string } | { kind: "none"; size: number };

export interface Routine {
  id: string;
  name: string;
  prompt: string;
  cron: string;
  source_file: string;
  enabled: boolean;
  paused: boolean;
  next_run_at?: string | null;
  last_run_at?: string | null;
  last_error?: string;
  created_by?: string;
}

export interface ConnectorInfo {
  id: string;
  name: string;
  description: string;
  available: boolean;
  note?: string;
  connected: boolean;
  account?: string;
  last_error?: string;
  tools: string[];
  connected_at?: string;
  can_setup: boolean;
  /** Connect opens the provider's sign-in; otherwise the person pastes a token. */
  sign_in: boolean;
  paste_token?: boolean;
  /** Creates the app at the provider, already filled in (Slack). */
  setup_url?: string;
  /** The sign-in return address an administrator registers with the provider. */
  redirect_uri?: string;
  /** Slack: "ready" when people can message their agents in Slack. */
  bot?: "ready" | "not_set_up";
}

/** A model a provider's key can use (for Lyzr, a Studio agent). */
export interface ModelChoice {
  id: string;
  name: string;
}

export interface ModelProviders {
  providers: { name: string; ready: boolean; source: "" | "server" | "app"; keyless: boolean; note?: string }[];
  default: string;
  can_set: boolean;
}

/** What an agent reports about its ongoing work (lux__update_status). */
export interface WorkStatus {
  job?: string;
  doing: string;
  waiting_on?: string[];
  decisions?: string[];
  deadlines?: { what: string; when: string }[];
}

export interface AgentWork {
  status: { status: WorkStatus; task_id?: string; updated_at: string } | null;
  follow_ups: Routine[];
  routines: Routine[];
}

export interface AgentMemory {
  text: string;
  editable: boolean;
}
