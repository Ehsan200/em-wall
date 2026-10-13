<script lang="ts" setup>
import { ref, watch, computed, onMounted, onUnmounted } from 'vue';
import type { ipc } from '../../wailsjs/go/models';

const props = defineProps<{
  status: ipc.StatusResult | null;
  error: string;
  appVersion: string;
  incompatible?: boolean;
  daemonVersion?: string;
  egress?: ipc.ExitIPResult | null;
  egressLoading?: boolean;
  // False until the app's first Status() call returns — "connecting", not
  // "unreachable".
  booted?: boolean;
}>();

const emit = defineEmits<{ (e: 'refresh-egress'): void }>();

// Uptime ticks every second locally. Status() arrives every ~2s, so
// showing its value directly made the counter jump 2–3s at a time. We
// anchor on the daemon's uptimeSec at the moment each reply landed and add
// the wall time since; each new reply re-anchors, so drift can't build up.
const now = ref(Date.now());
let anchor: { sec: number; at: number } | null = null;
const elapsed = (a: { sec: number; at: number }, t: number) => a.sec + Math.max(0, Math.floor((t - a.at) / 1000));
watch(() => props.status?.uptimeSec, (sec) => {
  const t = Date.now();
  if (typeof sec !== 'number') { anchor = null; }
  // Within a second of what we already show: keep the old anchor, or
  // reply latency and rounding would make the counter step back by one.
  // A bigger gap (daemon restarted, laptop slept) re-anchors.
  else if (!anchor || Math.abs(elapsed(anchor, t) - sec) > 1) { anchor = { sec, at: t }; }
  now.value = t;
}, { immediate: true });
let ticker: number | undefined;
onMounted(() => { ticker = window.setInterval(() => { now.value = Date.now(); }, 1000); });
onUnmounted(() => { if (ticker) window.clearInterval(ticker); });

// Same shape as Go's time.Duration.String() for whole seconds ("13m13s",
// "1h0m5s") so it matches the CLI.
function fmtDuration(total: number): string {
  const h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
  if (h) return `${h}h${m}m${s}s`;
  if (m) return `${m}m${s}s`;
  return `${s}s`;
}
const uptime = computed(() => {
  // An older daemon without uptimeSec: show its string as before.
  if (!anchor) return props.status?.uptime ?? '';
  return fmtDuration(elapsed(anchor, now.value));
});

// Public IP is masked by default; click to reveal.
const egressRevealed = ref(false);

// Turn a 2-letter ISO country code into its flag emoji (regional
// indicator symbols). Empty/invalid codes render no flag.
function flagEmoji(cc?: string): string {
  if (!cc || cc.length !== 2) return '';
  const base = 0x1f1e6;
  const up = cc.toUpperCase();
  return String.fromCodePoint(base + up.charCodeAt(0) - 65, base + up.charCodeAt(1) - 65);
}

// maskIP hides the host-specific part of an IP address.
// IPv4: keeps first two octets → 104.16.*.*   IPv6: keeps first group.
function maskIP(ip?: string): string {
  if (!ip) return '';
  if (ip.includes(':')) return ip.split(':')[0] + ':…';
  const p = ip.split('.');
  return p.length === 4 ? `${p[0]}.${p[1]}.*.*` : ip;
}
</script>

<template>
  <div class="status-bar">
    <template v-if="status">
      <div><span class="label">Daemon</span> <span class="tag tag-allow">connected</span></div>
      <div><span class="label">Listen</span> {{ status.listenAddr }}</div>
      <div><span class="label">Upstream</span> {{ status.upstreamDns }}</div>
      <div><span class="label">DoH/DoT block</span>
        <span :class="status.blockEncryptedDns ? 'tag tag-block' : 'tag tag-off'">
          {{ status.blockEncryptedDns ? 'on' : 'off' }}
        </span>
      </div>
      <div><span class="label">Rules</span> {{ status.ruleCount }}</div>
      <div><span class="label">Uptime</span> {{ uptime }}</div>

      <!-- Public egress: what every site sees for traffic no rule matches. -->
      <div class="egress"
           :title="egress?.probed
             ? `Public IP — ${[egress.city, egress.region, egress.country].filter(Boolean).join(', ')} (default route). Click the IP to reveal.`
             : (egress?.message || 'fetching public IP…')">
        <span class="label">Egress</span>
        <template v-if="egress?.probed">
          <span v-if="flagEmoji(egress.country)" class="flag">{{ flagEmoji(egress.country) }}</span>
          <span class="ip" style="cursor: pointer"
                :title="egressRevealed ? 'click to mask' : 'click to reveal full IP'"
                @click="egressRevealed = !egressRevealed">
            {{ egressRevealed ? egress.exitIp : maskIP(egress.exitIp) }}
          </span>
          <span v-if="egress.city" class="muted">· {{ egress.city }}</span>
        </template>
        <span v-else-if="egress" class="muted">unknown</span>
        <span v-else class="skeleton inline" style="width: 6em"></span>
        <span v-if="egressLoading" class="spinner" title="Checking public IP…"></span>
        <button v-else class="egress-refresh" title="Refresh public IP" @click="emit('refresh-egress')">⟳</button>
      </div>

      <div v-if="incompatible" class="tag tag-block"
           :title="`daemon v${daemonVersion} ≠ app v${appVersion} — reinstall the daemon`">
        ⚠ daemon v{{ daemonVersion }} ≠ app v{{ appVersion }}
      </div>
      <div v-else class="muted">v{{ appVersion || status.version }}</div>
    </template>
    <template v-else>
      <div v-if="booted === false" class="muted"><span class="spinner"></span> connecting to daemon…</div>
      <div v-else><span class="tag tag-block">daemon unreachable</span> <span class="muted">{{ error || 'connecting…' }}</span></div>
      <div v-if="appVersion" style="margin-left:auto" class="muted">v{{ appVersion }}</div>
    </template>
  </div>
</template>

<style scoped>
.egress {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.egress .flag {
  font-size: 14px;
  line-height: 1;
}
.egress .ip {
  font-variant-numeric: tabular-nums;
}
.egress-refresh {
  border: none;
  background: transparent;
  cursor: pointer;
  padding: 0 2px;
  font-size: 13px;
  opacity: 0.6;
  color: inherit;
}
.egress-refresh:hover {
  opacity: 1;
}
</style>
