<script lang="ts" setup>
import { ref } from 'vue';
import type { ipc } from '../../wailsjs/go/models';

defineProps<{
  status: ipc.StatusResult | null;
  error: string;
  appVersion: string;
  incompatible?: boolean;
  daemonVersion?: string;
  egress?: ipc.ExitIPResult | null;
}>();

const emit = defineEmits<{ (e: 'refresh-egress'): void }>();

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
      <div><span class="label">Uptime</span> {{ status.uptime }}</div>

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
        <span v-else class="muted">{{ egress ? 'unknown' : '…' }}</span>
        <button class="egress-refresh" title="Refresh public IP" @click="emit('refresh-egress')">⟳</button>
      </div>

      <div v-if="incompatible" class="tag tag-block"
           :title="`daemon v${daemonVersion} ≠ app v${appVersion} — reinstall the daemon`">
        ⚠ daemon v{{ daemonVersion }} ≠ app v{{ appVersion }}
      </div>
      <div v-else class="muted">v{{ appVersion || status.version }}</div>
    </template>
    <template v-else>
      <div><span class="tag tag-block">daemon unreachable</span> <span class="muted">{{ error || 'connecting…' }}</span></div>
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
