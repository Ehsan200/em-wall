import { ref, type App } from 'vue';
import { LogError } from '../wailsjs/runtime/runtime';

// Frontend errors, caught in one place. A throw inside a Vue render leaves
// the component (often a whole panel) blank with no message anywhere — the
// WebView's console is not reachable in a packaged app. Every error caught
// here lands in a small in-memory ring (Logs → UI errors), raises a toast,
// and goes to the Wails app log.

export type UIError = {
  id: number;
  at: Date;
  source: 'render' | 'window' | 'promise';
  message: string;
  // Vue's lifecycle hint ("render function", "watcher callback", …) and the
  // component it came from, when Vue reported it.
  info: string;
  component: string;
  stack: string;
  count: number; // repeats of the same error folded into one row
};

const MAX = 100;
let seq = 0;

export const uiErrors = ref<UIError[]>([]);
// The latest error not yet dismissed — drives the toast.
export const uiErrorToast = ref<UIError | null>(null);

function describe(err: unknown): { message: string; stack: string } {
  if (err instanceof Error) return { message: err.message || err.name, stack: err.stack || '' };
  if (typeof err === 'string') return { message: err, stack: '' };
  try {
    return { message: JSON.stringify(err), stack: '' };
  } catch {
    return { message: String(err), stack: '' };
  }
}

export function recordUIError(source: UIError['source'], err: unknown, info = '', component = '') {
  const { message, stack } = describe(err);
  const list = uiErrors.value;
  // A render error re-throws on every re-render (each poll tick); fold
  // repeats into the newest row instead of flooding the list.
  const last = list[0];
  if (last && last.message === message && last.source === source && last.component === component) {
    last.count++;
    last.at = new Date();
    return;
  }
  const e: UIError = { id: ++seq, at: new Date(), source, message, info, component, stack, count: 1 };
  uiErrors.value = [e, ...list].slice(0, MAX);
  uiErrorToast.value = e;
  try {
    LogError(`[ui ${source}] ${component ? component + ': ' : ''}${message}${info ? ' (' + info + ')' : ''}\n${stack}`);
  } catch {
    /* outside Wails (vite preview): the ring is enough */
  }
}

// One-shot request for the Logs panel to open its UI errors view (the
// toast's Details button); LogsPanel consumes it on mount.
export const uiErrorsRequested = ref(false);
export function showUIErrors() {
  uiErrorsRequested.value = true;
  uiErrorToast.value = null;
}

export function clearUIErrors() {
  uiErrors.value = [];
  uiErrorToast.value = null;
}

export function installUIErrorCapture(app: App) {
  app.config.errorHandler = (err, instance, info) => {
    const name = (instance?.$options as any)?.__name || (instance?.$options as any)?.name || '';
    recordUIError('render', err, info, name);
    console.error(err);
  };
  window.addEventListener('error', (ev) => {
    recordUIError('window', ev.error ?? ev.message);
  });
  window.addEventListener('unhandledrejection', (ev) => {
    recordUIError('promise', ev.reason);
  });
}
