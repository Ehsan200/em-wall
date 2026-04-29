<script lang="ts" setup>
import { ref } from 'vue';
import { Install, WaitForDaemon } from '../../wailsjs/go/main/App';
import type { installer } from '../../wailsjs/go/models';

const props = defineProps<{
  status: installer.Status | null;
  packaged: boolean;
}>();

const emit = defineEmits<{ (e: 'installed'): void }>();

const busy = ref(false);
const error = ref('');
const stage = ref<'idle' | 'authorising' | 'starting'>('idle');

async function doInstall() {
  busy.value = true;
  error.value = '';
  stage.value = 'authorising';
  try {
    await Install();
    stage.value = 'starting';
    const ok = await WaitForDaemon(8000);
    if (!ok) {
      // Install script returned cleanly but the daemon isn't answering yet —
      // could be a slow-launching system, or pf rejected the config. We still
      // emit installed so the parent can re-probe; the user will see whatever
      // the next status check says.
      error.value = 'Daemon installed but did not start within 8s. Check Console.app for "em-walld".';
    }
    emit('installed');
  } catch (e: any) {
    const msg = e?.message || String(e);
    if (msg === 'cancelled') {
      // User clicked Cancel on the macOS auth prompt — silent, not an error.
    } else {
      error.value = msg;
    }
  } finally {
    busy.value = false;
    stage.value = 'idle';
  }
}

function bytes(n: number): string {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0; let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
}
</script>

<template>
  <div class="panel">
    <div class="install-card">
      <h2 style="margin-top: 0">Install em-wall</h2>

      <p class="muted" style="font-size: 13px; line-height: 1.55">
        em-wall runs as a privileged background daemon — it owns
        <code>127.0.0.1:53</code>, manages a small <code>pf</code> anchor, and
        installs per-host routes. To set this up, macOS will ask for your
        password.
      </p>

      <div class="col" style="gap: 4px; margin: 0 0 14px 0; padding: 10px 12px; background: rgba(255, 200, 110, 0.08); border: 1px solid rgba(255, 200, 110, 0.3); border-radius: 6px">
        <strong style="font-size: 12px">Before you ever delete this app</strong>
        <span class="muted" style="font-size: 11px; line-height: 1.5">
          Open <strong>Settings → Uninstall em-wall</strong> first. The daemon
          binary, LaunchDaemon plist, and DNS settings live <em>outside</em>
          this <code>.app</code> bundle, so dragging the app to Trash leaves a
          privileged process running with no UI to control it — and DNS
          pointing at <code>127.0.0.1</code> with nothing answering.
        </span>
      </div>

      <div class="col" style="gap: 4px; margin: 14px 0; padding: 12px; background: var(--bg-2, rgba(255,255,255,0.03)); border: 1px solid var(--border); border-radius: 6px">
        <div class="muted" style="font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px">Will install</div>
        <div class="row" style="gap: 8px"><code>/usr/local/bin/em-walld</code><span class="muted" style="font-size: 11px">— the daemon binary</span></div>
        <div class="row" style="gap: 8px"><code>/Library/LaunchDaemons/com.em-wall.daemon.plist</code><span class="muted" style="font-size: 11px">— start at boot</span></div>
        <div class="row" style="gap: 8px"><code>/etc/pf.anchors/em-wall</code><span class="muted" style="font-size: 11px">— DoH/DoT blocklist anchor</span></div>
        <div class="row" style="gap: 8px"><code>/etc/pf.conf</code><span class="muted" style="font-size: 11px">— anchor reference (existing file is backed up)</span></div>
        <div class="row" style="gap: 8px"><code>/usr/local/var/em-wall/</code><span class="muted" style="font-size: 11px">— rules database</span></div>
      </div>

      <div v-if="props.status && (props.status.binaryPresent || props.status.plistPresent || props.status.dbExists)"
           class="col" style="gap: 4px; margin: 0 0 14px 0; padding: 10px 12px; background: rgba(255, 200, 110, 0.08); border: 1px solid rgba(255, 200, 110, 0.3); border-radius: 6px">
        <strong style="font-size: 12px">Existing installation detected</strong>
        <div class="muted" style="font-size: 11px">
          <span v-if="props.status.binaryPresent">Daemon binary present.</span>
          <span v-if="props.status.plistPresent"> LaunchDaemon plist present.</span>
          <span v-if="props.status.dbExists"> Rules DB ({{ bytes(props.status.dbSizeBytes) }}) will be reused.</span>
          <span v-if="!props.status.daemonRunning"> Daemon not running.</span>
        </div>
        <div class="muted" style="font-size: 11px">Re-installing is safe — the install script is idempotent.</div>
      </div>

      <div v-if="!props.packaged"
           class="col" style="gap: 4px; margin: 0 0 14px 0; padding: 10px 12px; background: rgba(255, 111, 111, 0.08); border: 1px solid rgba(255, 111, 111, 0.3); border-radius: 6px">
        <strong style="font-size: 12px">Development build</strong>
        <div class="muted" style="font-size: 11px; line-height: 1.5">
          This app build doesn't embed the daemon binary. Either build a full
          .app via <code>make app-bundle</code>, or run the daemon yourself
          via <code>make run-daemon</code>.
        </div>
      </div>

      <div v-if="error" class="error" style="margin-bottom: 12px">{{ error }}</div>

      <div class="row" style="gap: 10px">
        <button class="primary" :disabled="busy || !props.packaged" @click="doInstall">
          <template v-if="!busy">Install em-wall</template>
          <template v-else-if="stage === 'authorising'">Waiting for password…</template>
          <template v-else-if="stage === 'starting'">Starting daemon…</template>
          <template v-else>Installing…</template>
        </button>
        <span class="muted" style="font-size: 11px">macOS will prompt for your password.</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.install-card {
  max-width: 640px;
  margin: 32px auto;
}
code {
  font-size: 11px;
  padding: 1px 5px;
  border-radius: 3px;
  background: var(--panel);
  border: 1px solid var(--border);
}
</style>
