import { ref, computed } from 'vue';
import { XraySubExits, AckXraySubExits, CheckXraySubExits } from '../wailsjs/go/main/App';
import type { ipc } from '../wailsjs/go/models';

// Subscriptions as a rule exit ("xraysub:NAME", docs/xray-sub-exit.md).
//
// The daemon measures every subscription node's exit IP and reports a
// verdict per subscription: single (one exit IP — safe), multi (several, or
// a node whose IP rotates — sites see the user hop between addresses) or
// unverified (nothing measured yet). Binding a rule to a multi-exit
// subscription needs a confirmation: the daemon refuses with an error
// starting "multi-exit:", and withExitConfirm asks the user and retries
// with the confirmation flag.
//
// One shared store, so the Rules page, its banner and the Subscriptions
// panel show the same verdicts.

export const SUB_PREFIX = 'xraysub:';

const exits = ref<ipc.XraySubExitDTO[]>([]);
let loading: Promise<void> | null = null;

export async function loadSubExits(): Promise<void> {
  if (loading) return loading;
  loading = (async () => {
    try {
      exits.value = (await XraySubExits()) || [];
    } catch {
      /* keep the last answer — a hint, not worth an error banner */
    } finally {
      loading = null;
    }
  })();
  return loading;
}

export async function ackSubExits(id: number): Promise<void> {
  exits.value = (await AckXraySubExits(id)) || [];
}

// checkSubExits starts a measurement (0 = every subscription) and polls
// until the daemon reports it finished.
export async function checkSubExits(id: number): Promise<void> {
  exits.value = (await CheckXraySubExits(id)) || [];
  for (let i = 0; i < 90 && exits.value.some((e) => e.checking && (id === 0 || e.subId === id)); i++) {
    await new Promise((r) => setTimeout(r, 2000));
    await loadSubExits();
  }
}

export function useSubExits() {
  const byName = (name: string) => exits.value.find((e) => e.name === name);
  const byId = (id: number) => exits.value.find((e) => e.subId === id);
  // Subscriptions rules use whose exits changed past what the user accepted.
  const drifted = computed(() => exits.value.filter((e) => e.needsAck && e.ruleCount > 0));
  const options = computed(() =>
    exits.value.map((e) => ({
      value: e.name,
      label: e.name,
      hint: e.enabled ? verdictShort(e) : 'disabled',
      disabled: !e.enabled,
    })),
  );
  return { exits, byName, byId, drifted, options, load: loadSubExits };
}

export function ruleIsSub(field: string): boolean {
  return field.startsWith(SUB_PREFIX);
}

export function ruleSubName(field: string): string {
  return field.slice(SUB_PREFIX.length).trim();
}

export function countries(e: ipc.XraySubExitDTO): string[] {
  const out: string[] = [];
  for (const x of e.exits || []) if (x.country && !out.includes(x.country)) out.push(x.country);
  return out;
}

// verdictShort is the one-line label for pickers and chips.
export function verdictShort(e?: ipc.XraySubExitDTO): string {
  if (!e) return '?';
  if (e.checking && e.verdict === 'unverified') return 'checking…';
  switch (e.verdict) {
    case 'single': {
      const cc = e.exits?.[0]?.country;
      return `1 exit IP${cc ? ` · ${cc}` : ''}`;
    }
    case 'multi': {
      const cs = countries(e);
      return `${e.exits.length} exit IPs${cs.length ? ` · ${cs.join(', ')}` : ''}`;
    }
    default:
      return 'exits not checked yet';
  }
}

// errIsMultiExit matches the daemon's refusal for an unconfirmed
// multi-exit binding.
export function errIsMultiExit(e: unknown): boolean {
  const msg = (e as any)?.message || String(e);
  return msg.includes('multi-exit:');
}

// ---- confirmation dialog -------------------------------------------------
//
// One pending question at a time; ExitConfirmDialog renders it.

type Ask = { sub: string; message: string; resolve: (ok: boolean) => void };
export const exitAsk = ref<Ask | null>(null);

export function askExitConfirm(sub: string, message: string): Promise<boolean> {
  exitAsk.value?.resolve(false);
  return new Promise((resolve) => {
    exitAsk.value = { sub, message, resolve };
  });
}

export function answerExitConfirm(ok: boolean) {
  const a = exitAsk.value;
  exitAsk.value = null;
  a?.resolve(ok);
}

// withExitConfirm runs write(false); if the daemon refuses because the
// target subscription has several exit IPs, it asks the user and, on
// confirm, runs write(true). Resolves to false when the user cancels (the
// caller should then leave its form open). Other errors are rethrown.
export async function withExitConfirm(iface: string, write: (ack: boolean) => Promise<unknown>): Promise<boolean> {
  try {
    await write(false);
    return true;
  } catch (e) {
    if (!ruleIsSub(iface) || !errIsMultiExit(e)) throw e;
    await loadSubExits();
    const msg = ((e as any)?.message || String(e)).replace(/^.*?multi-exit:\s*/, '');
    if (!(await askExitConfirm(ruleSubName(iface), msg))) return false;
    await write(true);
    await loadSubExits();
    return true;
  }
}
