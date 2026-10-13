// startPoll runs fn every ms while the window is visible and once right
// away when it becomes visible again. A hidden or minimised window keeps
// its webview alive, and every tick is an IPC round trip to the daemon
// (often a SQLite read on the connection the DNS path also writes to), so
// polling nobody can see is pure cost. Returns a stop function.
export function startPoll(fn: () => unknown, ms: number): () => void {
  const tick = () => { if (!document.hidden) fn(); };
  const timer = window.setInterval(tick, ms);
  const onVisible = () => { if (!document.hidden) fn(); };
  document.addEventListener('visibilitychange', onVisible);
  return () => {
    window.clearInterval(timer);
    document.removeEventListener('visibilitychange', onVisible);
  };
}
