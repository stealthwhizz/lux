// Security settings for windows and navigation, kept in one place so they
// can be tested without starting Electron.

/** The renderer is served from this custom protocol, never from file://. */
export const APP_SCHEME = "app";
export const APP_ORIGIN = `${APP_SCHEME}://lux`;

export const webPreferences = {
  contextIsolation: true,
  nodeIntegration: false,
  nodeIntegrationInWorker: false,
  nodeIntegrationInSubFrames: false,
  sandbox: true,
  webSecurity: true,
  allowRunningInsecureContent: false,
  experimentalFeatures: false,
  webviewTag: false,
  spellcheck: true,
  safeDialogs: true,
} as const;

/**
 * Content-Security-Policy for the app's pages. The renderer gets no network
 * access at all: every request goes through the main process.
 */
export const CONTENT_SECURITY_POLICY = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self' 'unsafe-inline'", // React style attributes
  "img-src 'self' data:",
  "font-src 'self'",
  "connect-src 'none'",
  "frame-src 'none'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
].join("; ");

/** True only for the app's own pages. */
export function isAppURL(raw: string): boolean {
  try {
    const u = new URL(raw);
    return u.protocol === `${APP_SCHEME}:` && u.host === "lux";
  } catch {
    return false;
  }
}

/** Resolve an app:// request to a file inside the renderer directory, or null. */
export function resolveAppPath(raw: string, rendererDir: string, join: (...p: string[]) => string, normalize: (p: string) => string): string | null {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  if (!isAppURL(raw)) return null;
  let path = decodeURIComponent(u.pathname);
  if (path === "/" || path === "") path = "/index.html";
  if (path.includes("\0")) return null;
  const full = normalize(join(rendererDir, path));
  const root = normalize(rendererDir.endsWith("/") ? rendererDir : rendererDir + "/");
  if (!full.startsWith(root)) return null; // path traversal
  return full;
}

/** Electron permission requests the app grants (none: no camera, mic, geolocation, …). */
export function allowPermission(_permission: string): boolean {
  return false;
}
