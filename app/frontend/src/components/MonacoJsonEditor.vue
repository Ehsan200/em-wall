<script lang="ts" setup>
import { ref, watch, onMounted, onBeforeUnmount } from 'vue';
import * as monaco from 'monaco-editor';
import editorWorker from 'monaco-editor/esm/vs/editor/editor.worker?worker';
import jsonWorker from 'monaco-editor/esm/vs/language/json/json.worker?worker';

// Monaco needs to know how to spawn its language workers when running
// from a bundler. Vite's ?worker query gives us the per-language
// worker entry points; everything else uses the generic editor worker.
// MonacoEnvironment is process-global — setting it once at module load
// is intentional and idempotent (subsequent imports of this file are
// no-ops thanks to ESM caching).
;(self as any).MonacoEnvironment = {
  getWorker(_: unknown, label: string) {
    if (label === 'json') return new jsonWorker();
    return new editorWorker();
  },
};

const props = defineProps<{
  modelValue: string;
  height?: string;
  readonly?: boolean;
}>();

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void;
}>();

const container = ref<HTMLDivElement | null>(null);
let editor: monaco.editor.IStandaloneCodeEditor | null = null;
// Suppresses the change-event loop when we apply an external
// modelValue change back into the editor.
let applyingExternal = false;

onMounted(() => {
  if (!container.value) return;
  editor = monaco.editor.create(container.value, {
    value: props.modelValue ?? '',
    language: 'json',
    theme: 'vs-dark',
    automaticLayout: true,
    minimap: { enabled: false },
    fontSize: 12,
    tabSize: 2,
    scrollBeyondLastLine: false,
    readOnly: !!props.readonly,
    formatOnPaste: true,
  });

  editor.onDidChangeModelContent(() => {
    if (applyingExternal || !editor) return;
    emit('update:modelValue', editor.getValue());
  });
});

watch(() => props.modelValue, (v) => {
  if (!editor) return;
  if (editor.getValue() === v) return;
  applyingExternal = true;
  editor.setValue(v ?? '');
  applyingExternal = false;
});

watch(() => props.readonly, (ro) => {
  editor?.updateOptions({ readOnly: !!ro });
});

onBeforeUnmount(() => {
  editor?.dispose();
  editor = null;
});
</script>

<template>
  <div ref="container" :style="{ height: height || '320px', width: '100%', border: '1px solid var(--border)', borderRadius: '6px', overflow: 'hidden' }"></div>
</template>
