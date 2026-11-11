<script lang="ts" setup>
import { exitAsk, answerExitConfirm } from '../subExits';

// Asks before a rule is bound to a subscription whose nodes leave from more
// than one exit IP (docs/xray-sub-exit.md). Driven by withExitConfirm.
</script>

<template>
  <div v-if="exitAsk" class="overlay" @click.self="answerExitConfirm(false)">
    <div class="dialog" role="alertdialog" aria-labelledby="exit-confirm-title">
      <h3 id="exit-confirm-title" style="margin: 0 0 8px; color: var(--danger)">
        Route through “{{ exitAsk.sub }}” anyway?
      </h3>
      <p style="font-size: 12px; line-height: 1.55; margin: 0 0 8px">{{ exitAsk.message }}</p>
      <p class="muted" style="font-size: 11px; line-height: 1.55; margin: 0 0 14px">
        This subscription's nodes don't share one exit IP, so the IP sites see can change from one connection
        to the next — even inside one page load, because the balancer spreads connections over 2 nodes. Sign-ins,
        payment pages and anti-fraud checks may treat that as a hijacked session.
        If the subscription later gains a new exit IP, em-wall will ask again; your rules keep routing meanwhile.
        To keep one exit, pin a single node (Manual strategy) in the Subscriptions panel.
      </p>
      <div class="row" style="gap: 8px; justify-content: flex-end">
        <button @click="answerExitConfirm(false)">Cancel</button>
        <button class="danger-fill" @click="answerExitConfirm(true)">Use it anyway</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed; inset: 0; z-index: 60;
  background: rgba(0, 0, 0, 0.5);
  display: flex; align-items: center; justify-content: center;
}
.dialog {
  background: var(--panel); border: 1px solid rgba(255, 111, 111, 0.5); border-radius: 10px;
  padding: 18px; width: 460px; max-width: calc(100vw - 40px);
  box-shadow: 0 12px 40px rgba(0, 0, 0, 0.4);
}
.danger-fill { background: var(--danger); color: #fff; border-color: var(--danger); }
</style>
