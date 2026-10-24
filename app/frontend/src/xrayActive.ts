import { ref, onMounted, onUnmounted } from 'vue';
import { startPoll } from './poll';
import { XrayActive } from '../wailsjs/go/main/App';
import type { ipc } from '../wailsjs/go/models';

// Which pool node / outbound xray is picking right now (xray.active, read by
// the daemon from xray's access log). Shared by the Xray tab's entry rows,
// its subscription node table and the Health tab's pool timeline.

// useXrayActive polls xray.active every ms while the component is mounted
// and the window visible; when() gates individual ticks (e.g. only on the
// sub-tab that shows it). Errors leave the last answer in place: the
// indicator is a hint, not something to raise an error banner over.
export function useXrayActive(ms = 3000, when: () => boolean = () => true, history = false) {
  const active = ref<ipc.XrayActiveResult | null>(null);
  let stop: (() => void) | undefined;
  async function refresh() {
    if (!when()) return;
    try {
      active.value = await XrayActive(0, history);
    } catch {
      /* keep the last answer */
    }
  }
  onMounted(() => {
    refresh();
    stop = startPoll(refresh, ms);
  });
  onUnmounted(() => stop?.());
  return { active, refresh };
}

export function fmtAgo(sec: number): string {
  if (sec < 1) return 'just now';
  if (sec < 90) return `${Math.round(sec)}s ago`;
  return `${Math.round(sec / 60)}m ago`;
}

// pickText renders one member's line: "active · 4 conns/30s · picked 3s ago",
// or just "picked 45s ago" for a member that wasn't picked in the recent
// sub-window.
export function pickText(m: ipc.XrayActiveMemberDTO, recentSec: number): string {
  if (!m.active) return `picked ${fmtAgo(m.agoSec)}`;
  return `active · ${m.recent} conn${m.recent === 1 ? '' : 's'}/${recentSec}s · picked ${fmtAgo(m.agoSec)}`;
}

// The xray access log only sees connections being opened: a long-lived
// connection opened earlier may still ride another node.
export const PICK_NOTE = 'From xray’s access log: the node the balancer handed each new connection to. Connections opened earlier can still ride another node.';

// poolOf returns the pool a master rides (as owner or alias), if any.
export function poolOf(r: ipc.XrayActiveResult | null, master: string): ipc.XrayActivePoolDTO | undefined {
  const m = master.toLowerCase();
  return r?.pools.find(p => p.masters.some(x => x.toLowerCase() === m));
}
