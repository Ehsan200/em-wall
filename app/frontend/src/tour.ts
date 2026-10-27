import { reactive, watch } from 'vue';

// Content of the guided tour (AppTour.vue). Kept apart from the component
// so the copy can grow without touching the walk logic. Body strings may
// carry inline <code>/<b> — they are our own text, rendered with v-html.

export type Tab = 'rules' | 'suggest' | 'logs' | 'usage' | 'health' | 'map' | 'network' | 'proxies' | 'xray' | 'settings';

type FlowNode = { label: string; sub?: string; tone?: 'accent' | 'danger' | 'success' | 'warn' };

export type TourStep = {
  // Stable key. The app remembers which ids a user has seen, so a new id
  // is what makes a step show up in "What's new" — keep ids when rewording
  // a step, and give a step about a new feature a new one.
  id: string;
  chapter: string;
  title: string;
  // Tab to switch to before the step shows.
  tab?: Tab;
  // Sub-view inside that tab (Xray sub-tab, Logs view, Network section).
  // The panel applies it through onTourView.
  view?: string;
  // Selectors to spotlight, first one present wins. Page content loads
  // over IPC, so the tour keeps looking while the step is open; until a
  // target shows up (or when the page is empty) it falls back to the tab's
  // sidebar item. Omitted on a tab-less step = centered card.
  target?: string | string[];
  body: string[];
  // A left-to-right chain of boxes: how traffic or config flows.
  flow?: FlowNode[];
  // Alternatives after the flow: exactly one of them happens.
  fork?: FlowNode[];
  // Where the fork's branches meet again (all paths end at one place).
  join?: FlowNode;
  terms?: { term: string; def: string }[];
  tip?: string;
  // Red card and outline, for steps where getting it wrong hurts.
  danger?: boolean;
};


// Tour → panel hand-off for sub-views. A new object per request, so asking
// for the same view twice still fires; cleared when the tour closes so a
// later visit to the page opens on its usual view.
const tourView = reactive<Partial<Record<Tab, { v: string }>>>({});
export function setTourView(tab: Tab, v: string) { tourView[tab] = { v }; }
export function clearTourView() { for (const k of Object.keys(tourView) as Tab[]) delete tourView[k]; }
// Called from a panel's setup. immediate: the tour switches the tab first,
// so the panel mounts with the request already waiting.
export function onTourView(tab: Tab, apply: (v: string) => void) {
  watch(() => tourView[tab], (r) => { if (r) apply(r.v); }, { immediate: true });
}

const t = (name: string) => `[data-tour="${name}"]`;

const STEPS: TourStep[] = [
  // ───────────────────────── Welcome
  {
    chapter: 'Welcome',
    id: 'welcome',
    title: 'em-wall in one minute',
    body: [
      'em-wall is a firewall that works at the <b>DNS layer</b>. Every time an app looks up a name, the lookup comes to em-wall first, and your rules decide what happens to it. Rules can also match <b>IP addresses and ranges</b>, for apps that connect without looking up a name.',
      'The heavy lifting is done by a background service (the daemon) that runs as root. This app, and the <code>em-wall</code> command in Terminal, only talk to it. <b>Quitting the app does not turn protection off</b>: em-wall keeps working in the background.',
    ],
    flow: [
      { label: 'App asks for a name' },
      { label: 'em-wall', sub: 'matches your rules', tone: 'accent' },
    ],
    fork: [
      { label: 'Block', sub: 'name does not exist', tone: 'danger' },
      { label: 'Allow', sub: 'normal internet', tone: 'success' },
      { label: 'Route', sub: 'tunnel or proxy', tone: 'accent' },
    ],
    tip: 'Use ← → or Enter to move, Esc to leave. The <b>Tour</b> menu at the bottom of the sidebar opens any section again later.',
  },
  {
    chapter: 'Welcome',
    id: 'status-bar',
    title: 'Status bar',
    target: '.status-bar',
    body: [
      'A quick health check. <b>Daemon</b> shows whether the background service is reachable. <b>Upstream</b> is the real DNS server em-wall forwards allowed lookups to. <b>DoH/DoT block</b> shows whether encrypted DNS is being stopped. <b>Egress</b> is your public IP and where it appears to be. Click it to check again.',
      'If the app and daemon versions ever go out of step, a banner appears here with a <b>Reinstall</b> button. App updates appear here too.',
    ],
  },
  {
    chapter: 'Welcome',
    id: 'sidebar',
    title: 'Finding your way',
    target: 'nav.sidebar',
    body: [
      'The pages are grouped by job: <b>Routing</b> decides where names go, <b>Outbounds</b> are the servers and proxies traffic can leave through, <b>Monitor</b> shows what is happening, and <b>System</b> controls how em-wall hooks into macOS.',
      'A number on <b>Suggestions</b> means em-wall has spotted services you might want a rule for.',
    ],
  },
  {
    id: 'settings-uninstall',
    chapter: 'Welcome',
    title: 'Uninstall: always from Settings first',
    tab: 'settings',
    target: t('settings-uninstall'),
    danger: true,
    body: [
      '<b>Never drag em-wall to the Trash while it is still installed.</b>',
      'The background service doesn’t live inside the app. Trash only the app and the service <b>keeps running and keeps controlling your Mac’s DNS</b>, with no app left to turn it off. If it ever stops after that, <b>websites stop loading</b> until DNS is fixed by hand.',
    ],
    flow: [
      { label: '1. Settings → Uninstall', tone: 'danger' },
      { label: '2. DNS put back', sub: 'service removed', tone: 'success' },
      { label: '3. Trash the app' },
    ],
    tip: '<b>Uninstall</b> is at the bottom of this page. It puts your original DNS settings back and removes the background service. <b>Only then</b> drag the app to the Trash.',
  },

  // ───────────────────────── Use cases
  // What em-wall itself is for, not what xray can do. Each card points at
  // the page where the idea lives, without switching to it.
  {
    id: 'usecase-split',
    chapter: 'Use cases',
    title: 'Each site takes its own path',
    target: '[data-tour="nav-rules"]',
    body: [
      'You choose, per domain, what goes direct, what goes through which server, and what’s blocked, instead of one VPN for everything. Local sites like your bank stay on your home connection.',
    ],
    flow: [
      { label: 'Any site', sub: 'by name or IP' },
      { label: 'em-wall', sub: 'your rules', tone: 'accent' },
    ],
    fork: [
      { label: '*.anthropic.com', sub: '→ xrayset:us', tone: 'accent' },
      { label: '*.netflix.com', sub: '→ xray:uk', tone: 'accent' },
      { label: 'everything else', sub: '→ direct', tone: 'success' },
    ],
  },
  {
    id: 'usecase-ip',
    chapter: 'Use cases',
    title: 'Route by IP, not only by name',
    target: '[data-tour="nav-rules"]',
    body: [
      'Some apps connect to raw IP addresses and never look up a name: Telegram, games, some Google services. An IP or range rule catches them too, and works with every target: interface, proxy, xray or set.',
    ],
    flow: [
      { label: '91.108.0.0/16', sub: 'Telegram' },
      { label: 'xrayset:us', tone: 'accent' },
    ],
  },
  {
    id: 'usecase-private',
    chapter: 'Use cases',
    title: 'Reach a private network',
    target: '[data-tour="nav-rules"]',
    body: [
      'Reach an office or home network through a server inside it. Internal names are looked up <b>on the far side</b>, so <code>jira.corp.internal</code> works from anywhere, and the network’s IP ranges can go the same way.',
    ],
    flow: [
      { label: '*.corp.internal', sub: '+ 10.20.0.0/16' },
      { label: 'xray:office', sub: 'server inside the network', tone: 'accent' },
      { label: 'Office network', tone: 'success' },
    ],
  },
  {
    id: 'usecase-block',
    chapter: 'Use cases',
    title: 'Block system-wide',
    target: '[data-tour="nav-rules"]',
    body: [
      'Block ads, trackers, telemetry, or distracting sites for every app on the Mac, including apps with no settings of their own. Encrypted DNS is blocked too, so apps can’t get around it.',
    ],
  },
  {
    id: 'usecase-private-lookups',
    chapter: 'Use cases',
    title: 'Your ISP doesn’t see where routed traffic goes',
    target: '[data-tour="nav-rules"]',
    body: [
      'Routed names are never looked up on your local network. em-wall answers with a placeholder address and the real lookup happens on the far side, so neither the lookup nor the site name crosses your ISP.',
    ],
  },
  {
    id: 'usecase-no-leak',
    chapter: 'Use cases',
    title: 'Routed traffic never leaks',
    target: '[data-tour="nav-xray"]',
    body: [
      'If a route’s server is down or turned off, the rule <b>fails</b> instead of quietly using your normal connection. A site you meant to send through a server never shows up from your own IP.',
    ],
  },
  {
    id: 'usecase-resilience',
    chapter: 'Use cases',
    title: 'Stay online on bad networks',
    target: '[data-tour="nav-xray"]',
    body: [
      'Put many servers behind one name. em-wall keeps testing them, moves away from dead or flaky ones by itself, and keeps each site on one exit so logins and sessions don’t break.',
    ],
  },
  {
    id: 'usecase-services',
    chapter: 'Use cases',
    title: 'Know which services you use',
    target: '[data-tour="nav-suggest"]',
    body: [
      'Ready-made groups list every domain a service uses, so one click covers all of it. em-wall also notices services your Mac keeps reaching and suggests groups for them.',
    ],
  },
  {
    id: 'usecase-observe',
    chapter: 'Use cases',
    title: 'See what your Mac talks to',
    target: '[data-tour="group-Monitor"]',
    body: [
      'Every lookup is listed with the rule that decided it. A live map shows where routed traffic goes, usage shows totals per route, and health shows whether a problem is the server or your own line.',
    ],
  },
  {
    id: 'usecase-safe',
    chapter: 'Use cases',
    title: 'Safe to turn on and remove',
    target: '[data-tour="nav-settings"]',
    body: [
      'Your DNS settings are saved before em-wall takes over, a working DNS server is checked first, and uninstalling from <b>Settings</b> puts everything back.',
    ],
  },
  {
    id: 'usecase-cli',
    chapter: 'Use cases',
    title: 'Scriptable',
    body: [
      'The <code>em-wall</code> command does status, rules, groups and health from Terminal, with no sudo, and its exit codes can be used in scripts.',
    ],
    terms: [
      { term: 'em-wall status', def: 'Is the daemon up, which DNS it uses.' },
      { term: 'em-wall rules list', def: 'Every rule, its action and target.' },
      { term: 'em-wall health', def: 'Connection health and pool state.' },
    ],
  },
  {
    id: 'usecase-limits',
    chapter: 'Use cases',
    title: 'Limits today',
    body: [
      'Worth knowing before you plan around em-wall:',
    ],
    terms: [
      { term: 'VPN apps', def: 'VPN apps that take over DNS don’t work alongside em-wall.' },
      { term: 'IP blocking', def: 'Blocking by IP isn’t enforced yet (it’s on the roadmap). Route by IP works.' },
      { term: 'utunN routes', def: 'A tunnel interface route applies to the whole IP address, so other sites on a shared CDN address go along with it. Proxy, xray and set targets don’t have this problem.' },
    ],
  },

  // ───────────────────────── Routing
  {
    chapter: 'Rules',
    id: 'rules-page',
    title: 'Rules page',
    tab: 'rules',
    target: t('rules-search'),
    body: [
      'Everything about what gets blocked or routed lives here. The search box filters the groups and the rule list below at the same time. Try a service name or a pattern like <code>*.openai.com</code>.',
      'Names that match no rule are <b>allowed</b> by default, so an empty list changes nothing.',
    ],
  },
  {
    chapter: 'Rules',
    id: 'rules-quick',
    title: 'Quick add a whole service',
    tab: 'rules',
    target: t('rules-quick'),
    body: [
      'Each card is a ready-made <b>group</b>: every domain a service uses, for example “Anthropic” or “OpenAI”. Click one, choose <b>Block</b> or <b>Route</b> and a target, and em-wall creates one rule per domain.',
      'Those rules are normal rules. They show up together under the group’s name in the list, and you can edit or turn off each one on its own.',
    ],
  },
  {
    chapter: 'Rules',
    id: 'rules-custom',
    title: 'Your own groups',
    tab: 'rules',
    target: t('rules-custom'),
    body: [
      '<b>+ New group</b> saves your own bundle of patterns (a work tool, a game, a set of sites) so you can apply it in one click like a built-in group. ✎ edits it.',
      'Suggestions you accept land here too.',
    ],
  },
  {
    chapter: 'Rules',
    id: 'rules-add',
    title: 'Add a single rule',
    tab: 'rules',
    target: t('rules-add'),
    body: [
      'Type a pattern, pick an action, and (for Route) a target: an <b>Interface</b>, one or more <b>Proxies</b>, or one or more <b>Xray</b> outbounds. When you pick several, the number on each chip is its order: the first one that works is used.',
    ],
    terms: [
      { term: 'example.com', def: 'Only that exact name.' },
      { term: '*.example.com', def: 'The name itself <b>and</b> every subdomain under it.' },
      { term: '1.2.3.4', def: 'One destination IP.' },
      { term: '10.0.0.0/8', def: 'A whole IP range. IP rules work with <b>Route</b>: traffic to those addresses goes through the target (interface, proxy, xray or set), even when the app never looked up a name. Typical uses are a private network behind a VPN, or a service such as Telegram that connects to its servers by IP.' },
    ],
  },
  {
    chapter: 'Rules',
    id: 'rules-specificity',
    title: 'Which rule wins',
    tab: 'rules',
    target: t('rules-add'),
    body: [
      'When several rules match, <b>the most specific one wins</b>. An exact name beats a wildcard, and a deeper wildcard beats a shallower one. Order in the list does not matter.',
      'So <code>*.google.com</code> → Route and <code>mail.google.com</code> → Allow sends all of Google through the tunnel except Mail.',
    ],
    terms: [
      { term: 'Block', def: 'Answers “that name doesn’t exist”, so the app never connects.' },
      { term: 'Allow', def: 'Normal internet. Mostly useful as an exception inside a wider Block or Route.' },
      { term: 'Route', def: 'Sends the traffic out through the chosen target instead of your normal connection.' },
    ],
  },
  {
    chapter: 'Rules',
    id: 'rules-targets',
    title: 'Route targets and FakeIP',
    tab: 'rules',
    target: t('rules-add'),
    body: [
      'A routed rule stores its target as text. You will see these forms across the app:',
    ],
    terms: [
      { term: 'utun3', def: 'A network interface, such as a VPN tunnel. Find names on the Network page.' },
      { term: 'proxy:NAME', def: 'A SOCKS5 / HTTP proxy from the Proxies page.' },
      { term: 'xray:NAME', def: 'An xray outbound from the Xray page.' },
      { term: 'xray:a,b', def: 'Several targets. The first one that works is used.' },
      { term: 'xrayset:NAME', def: 'An outbound set (covered in the Xray chapter).' },
    ],
    tip: 'For proxy and xray targets em-wall answers with a placeholder address (<b>FakeIP</b>, 198.18.x.x) without looking the name up. The real lookup happens on the far side, so your ISP never sees which sites you route.',
  },
  {
    chapter: 'Rules',
    id: 'rules-list',
    title: 'The rule list',
    tab: 'rules',
    target: [t('rules-list'), t('rules-add')],
    body: [
      'Rules are grouped in sections by the group they came from. Click a section header to fold it, and use its checkbox to select the whole section. The switch on each row turns a rule on or off without deleting it. <b>Edit</b> changes pattern, action or target.',
      'Each routed rule shows the <b>exit IP</b> its traffic leaves from, with the country. ↻ checks it again.',
    ],
    tip: 'Select several rules and a toolbar appears: <b>Change to…</b> edits all of them at once, <b>Export selected…</b> saves them to an encrypted file, and <b>Move to group</b> turns them into a custom group.',
  },
  {
    chapter: 'Rules',
    id: 'rules-sync',
    title: 'When a group gets new domains',
    tab: 'rules',
    target: [t('rules-sync'), t('rules-quick')],
    body: [
      'Built-in groups grow with app updates, but rules you already applied don’t change by themselves. When a group you use has new domains, a banner and a <b>+N new</b> pill on its card show up.',
      '<b>Sync</b> adds just the missing domains, with the same action and target as the rest of that group. Nothing else is touched.',
    ],
  },
  {
    chapter: 'Rules',
    id: 'suggestions',
    title: 'Suggestions',
    tab: 'suggest',
    target: [t('suggest-list')],
    body: [
      'em-wall watches the names <b>no rule matched</b> and checks which known service each one belongs to (from the geosite list). Once a service keeps showing up over time, not just in one page load, it appears here with the names it saw.',
      'Pick one on the left. On the right: <b>Create group</b> saves it as a custom group, <b>Apply …</b> opens the matching built-in group instead, <b>Not now</b> hides it for 7 days, and the last button hides it for good.',
    ],
    tip: 'Accepting a suggestion never changes routing by itself. You still choose an action when you apply the group. Shared CDN domains (Cloudflare, CloudFront…) are never counted, so routing one company doesn’t route every site behind its CDN.',
  },

  // ───────────────────────── Xray
  {
    chapter: 'Xray',
    id: 'xray-page',
    title: 'Xray page',
    tab: 'xray',
    view: 'outbounds',
    target: t('xray-subtabs'),
    body: [
      'xray is the engine that carries traffic to remote servers. This page has five tabs:',
    ],
    terms: [
      { term: 'Outbounds', def: 'Your servers, one entry each.' },
      { term: 'Sets', def: 'Named bundles of outbounds that rules can share.' },
      { term: 'Subscriptions', def: 'Provider URLs that supply whole pools of servers.' },
      { term: 'Routing', def: 'Advanced xray rules, as JSON.' },
      { term: 'Status', def: 'The xray process and its log.' },
    ],
    tip: 'The header shows the xray version and whether it is running. <b>Test target</b> is the host:port that every Test button connects to.',
  },
  {
    chapter: 'Xray',
    id: 'xray-add',
    title: 'Add an outbound',
    tab: 'xray',
    view: 'outbounds',
    target: t('xray-add'),
    body: [
      'An <b>outbound</b> is one server you can send traffic through: VLESS, VMess, Trojan or Shadowsocks. <b>Import</b> takes a share link (<code>vless://…</code>) or JSON. <b>+ Add outbound</b> opens the JSON editor. A rule then targets it as <code>xray:NAME</code>.',
      '<b>Test all</b> checks every enabled outbound against the test target.',
    ],
    tip: 'Changes take effect <b>live</b>. xray isn’t restarted for a routine edit, so connections that are already open keep working.',
  },
  {
    chapter: 'Xray',
    id: 'xray-row',
    title: 'An outbound row',
    tab: 'xray',
    view: 'outbounds',
    target: [t('xray-entry'), t('xray-add')],
    body: [
      'Each row shows the protocol, server, and the local port xray listens on for it. <b>Test</b> measures it, <b>Share</b> copies it as a link, <b>Edit</b> opens the editor, and <b>Disable</b> takes it out of use without deleting it.',
      'While traffic flows, a badge shows which server or pool node it is going through right now. Search and the filters above the list narrow a long list.',
    ],
    terms: [
      { term: 'Mux', def: 'Carries many connections inside one tunnel, skipping a handshake for each new connection. Helps most on slow links. Set in Edit.' },
      { term: 'Fragment', def: 'Splits the opening TLS packet into pieces, to get past filters that look at it. Set in Edit.' },
    ],
    tip: 'Tick several rows to <b>Change dialer…</b> on all of them at once.',
  },
  {
    chapter: 'Xray',
    id: 'xray-subscriptions',
    title: 'Subscriptions and pool nodes',
    tab: 'xray',
    view: 'subscriptions',
    target: [t('xray-sub'), t('xray-sub-add')],
    body: [
      'A <b>subscription</b> is a URL from your provider that lists many servers. Each of those servers is a <b>pool node</b>, and together they make up the subscription’s <b>pool</b>. <b>+ Add subscription</b> to start, and <b>Refresh</b> (or Refresh all) to fetch the current list.',
      'Pool nodes are <b>not</b> rule targets on their own. They exist to carry a <b>master</b> (next step). <b>Nodes</b> opens the list with each node’s ping. ★ marks the nodes carrying traffic right now.',
    ],
    flow: [
      { label: 'Subscription URL', sub: 'from your provider' },
      { label: 'Pool', sub: 'node 1 · node 2 · … node N', tone: 'accent' },
      { label: 'Master', sub: 'dials through a pool node', tone: 'warn' },
    ],
    tip: 'Want to use one node directly? Its <b>+ Add</b> button makes an independent copy as a normal outbound, so later refreshes can’t break it.',
  },
  {
    chapter: 'Xray',
    id: 'xray-master',
    title: 'Master: a server reached through a pool',
    tab: 'xray',
    view: 'outbounds',
    target: [t('xray-entry'), t('xray-add')],
    body: [
      'A <b>master</b> is an outbound with a <b>Dialer</b> set (in Edit). Instead of connecting to its server directly, it connects <b>through a pool node</b>. In the list it shows a <code>master · NAME</code> badge.',
      'Websites only ever see the <b>master’s</b> server. The pool node is just the road to it, so your exit IP stays the same whichever node is carrying the connection.',
    ],
    flow: [
      { label: 'Your Mac' },
      { label: 'Pool node', sub: 'picked by em-wall', tone: 'warn' },
      { label: 'Master server', sub: 'your exit IP', tone: 'accent' },
      { label: 'Website', tone: 'success' },
    ],
    terms: [
      { term: 'Shortlist', def: 'The best 2 nodes (judged on speed and reliability over time) that carry a master.' },
      { term: 'Parked', def: 'A node that has stayed dead for a while is left out, then retried after 5 min, doubling up to 1 h.' },
      { term: 'Broken', def: 'A node that answers health checks but stalls on real downloads. Ranked last and never chosen.' },
    ],
    tip: 'Masters with the same Dialer share one pool and one set of health checks. Sharing a master as a link exports only its own server, not the pool.',
  },
  {
    chapter: 'Xray',
    id: 'xray-strategy',
    title: 'Switch strategy',
    tab: 'xray',
    view: 'subscriptions',
    target: [t('xray-strategy'), t('xray-sub-add')],
    body: [
      'Each subscription chooses how its pool picks nodes:',
    ],
    terms: [
      { term: 'Auto', def: 'The default. Runs Stable, switches to Agile on its own while nodes come and go in waves, and returns to Stable after 30 calm minutes.' },
      { term: 'Stable', def: 'For nodes that stay up (or down) for long stretches. Keeps a steady pair of nodes and parks dead ones.' },
      { term: 'Agile', def: 'For nodes that work for a minute, then drop. Spreads traffic over every node answering right now (up to 4) and never parks.' },
      { term: 'Manual', def: 'Only the nodes you <b>Pin</b> in the node list.' },
    ],
    tip: 'Not sure? Leave it on Auto, and look at the Pool timeline on the Health page if a master feels unsteady.',
  },
  {
    chapter: 'Xray',
    id: 'xray-sets',
    title: 'Outbound sets',
    tab: 'xray',
    view: 'sets',
    target: [t('xray-set'), t('xray-sets-new')],
    body: [
      'A <b>set</b> is a named, ordered list of outbounds that a rule targets as <code>xrayset:NAME</code>. A typical use: <b>two xray configs with different settings that reach the same server</b>, say one over a CDN with TLS and one direct with REALITY. If your network blocks one way in, traffic carries on over the other, and sites see <b>the same exit IP</b> either way.',
      'em-wall chooses a member for each site and <b>keeps using it</b>. It only moves when that member fails or another one is clearly better. Change a set’s members and every rule using it follows. <b>+ New set</b> creates one, and each card shows how many members can be used right now.',
    ],
    flow: [
      { label: '*.github.com', sub: 'xrayset:home' },
    ],
    fork: [
      { label: 'xray:home-cdn', sub: 'TLS via a CDN', tone: 'accent' },
      { label: 'xray:home-reality', sub: 'REALITY, direct', tone: 'accent' },
    ],
    join: { label: 'Your server', sub: 'one exit IP', tone: 'success' },
    terms: [
      { term: 'down', def: 'Several failures in a row. Back as soon as it answers again.' },
      { term: 'demoted', def: 'Fails often, or keeps going up and down. Held out for a cooldown period.' },
      { term: 'probation', def: 'Just recovered. Placed behind steady members for a while.' },
    ],
    tip: 'A disabled set makes its rules <b>block</b> instead of falling back to your normal connection, so traffic you meant to route never leaks out directly.',
  },
  {
    chapter: 'Xray',
    id: 'xray-routing',
    title: 'Routing (advanced)',
    tab: 'xray',
    view: 'routing',
    target: t('xray-routing'),
    body: [
      'Extra xray routing rules as a JSON array, checked before the normal one-entry-one-server setup. Use <code>geosite:TAG</code> / <code>geoip:CC</code> to match lists, for example to send a country’s sites straight out from the server.',
      '<b>Format</b> tidies the JSON, <b>Save</b> applies it live. Most setups never need this. em-wall rules cover the usual cases.',
    ],
  },
  {
    chapter: 'Xray',
    id: 'xray-status',
    title: 'Status',
    tab: 'xray',
    view: 'status',
    target: t('xray-status'),
    body: [
      'Whether the xray process is running, its version, the ports it listens on, its log file, why it last exited, and its most recent output. Look here first when every xray target fails at once.',
    ],
  },
  {
    chapter: 'Xray',
    id: 'proxies',
    title: 'Proxies',
    tab: 'proxies',
    target: t('proxies-list'),
    body: [
      'Plain <b>SOCKS5</b> or <b>HTTP</b> proxies, for example an SSH tunnel (<code>ssh -D</code>) or a proxy at work. <b>+ Add proxy</b>, then <b>Test</b> to check it.',
      'Rules target them as <code>proxy:NAME</code>, and they can be members of a set next to xray outbounds.',
    ],
  },

  // ───────────────────────── Monitor
  {
    chapter: 'Monitor',
    id: 'map',
    title: 'Map',
    tab: 'map',
    target: t('map-head'),
    body: [
      'Live routed connections drawn on a world map: from your Mac, through the pool node and master if there are any, to the city of the destination. Filter by group or set. <b>Fit paths</b> zooms to the active lines and <b>World</b> shows the whole map.',
      'Only routed TCP traffic appears. The list beside the map shows each flow with its speed. Hover or click one to highlight its path.',
    ],
  },
  {
    chapter: 'Monitor',
    id: 'usage',
    title: 'Usage',
    tab: 'usage',
    target: t('usage-toolbar'),
    body: [
      'How much data went through each route over a time range. Group it by <b>Group</b>, <b>Domain</b> or <b>Proxy</b> to see what is using a tunnel.',
      'It counts routed (proxy / xray) traffic only. Blocked names and plain interface routes don’t pass through em-wall’s data path.',
    ],
  },
  {
    chapter: 'Monitor',
    id: 'health-conn',
    title: 'Health: connections',
    tab: 'health',
    target: [t('health-conn')],
    body: [
      'The last 15 minutes of routed connections. The tiles show how many failed and why, how long setup took, how often a second server had to be raced, and how often sites were moved to a different exit (<b>Exit switches</b>).',
      'Below: failures by cause, each outbound’s carried vs. blamed connections, and any parked pool nodes.',
    ],
    tip: 'Before changing a setting because something feels slow, check here first. It usually shows whether the problem is a node, a master, or your own connection.',
  },
  {
    chapter: 'Monitor',
    id: 'health-pools',
    title: 'Health: pool timeline',
    tab: 'health',
    target: [t('health-pools'), t('health-conn')],
    body: [
      'For every master: what each of its pool nodes did over the last 30 minutes (up, down, shortlisted, carrying data) next to how the master itself was doing.',
      'If the nodes are steady while the master drops, the master’s own server is the problem. If the nodes flap, the pool is. Gaps where nothing answered were your own uplink and are left out.',
    ],
  },
  {
    chapter: 'Monitor',
    id: 'logs',
    title: 'Logs',
    tab: 'logs',
    view: 'dns',
    target: t('logs-tabs'),
    body: [
      '<b>DNS decisions</b> lists every lookup with the rule that decided it. Filter by domain or by All / Block / Route to find why a site behaves the way it does.',
      '<b>Daemon</b>, <b>Xray errors</b> and <b>Xray access</b> show the raw logs, which are useful when reporting a problem.',
    ],
  },

  // ───────────────────────── System
  {
    chapter: 'System',
    id: 'network',
    title: 'Network',
    tab: 'network',
    view: 'interfaces',
    target: t('network-sections'),
    body: [
      '<b>Interfaces</b>: every network interface on your Mac with its addresses. Tunnels (<code>utunN</code>) are the names you use as Interface targets. <b>Routes</b>: the system routing table. <b>Pinned</b>: the per-address routes em-wall has added for Route rules, and when each one expires.',
    ],
  },
  {
    chapter: 'System',
    id: 'settings-hijack',
    title: 'System DNS hijack',
    tab: 'settings',
    target: t('settings-hijack'),
    body: [
      'This is the main switch. When on, every network service’s DNS points at em-wall, which is how all apps end up going through it. When off, rules don’t apply and nothing is logged.',
      'Your original DNS settings are saved first and put back when you turn it off. em-wall also checks that it can reach a real DNS server before switching, so it won’t leave you offline.',
    ],
  },
  {
    chapter: 'System',
    id: 'settings-doh',
    title: 'Block encrypted DNS',
    tab: 'settings',
    target: t('settings-doh'),
    body: [
      'Browsers and some apps can use DNS-over-HTTPS / TLS and skip em-wall entirely. This blocks those servers at the firewall, so those apps fall back to normal DNS and your rules apply to them too.',
    ],
  },
  {
    id: 'settings-updates',
    chapter: 'System',
    title: 'Update reminders',
    tab: 'settings',
    target: '[data-tour="settings-updates"]',
    body: [
      'em-wall keeps running in the background even when you quit the app. When a new version is out and the app isn’t open, it asks you once with an <b>Open em-wall</b> button, which opens the app so you can install it. After installing, a short tour shows what’s new.',
      'Turn the reminder off here. <code>em-wall status</code> in Terminal also mentions a new version.',
    ],
  },
  {
    chapter: 'System',
    id: 'settings-geo',
    title: 'Geo data and suggestions',
    tab: 'settings',
    target: [t('settings-geo'), t('settings-suggest')],
    body: [
      '<b>Geo data</b> holds the lists of which domains and IPs belong to which service or country. Suggestions, the map and xray’s <code>geosite:</code> rules use them. They update by themselves; <b>Update all now</b> updates them right away.',
      '<b>Group suggestions</b> (just below) turns suggestions on or off and brings back ones you hid.',
    ],
  },
  {
    chapter: 'System',
    id: 'settings-backup',
    title: 'Backup, reinstall, uninstall',
    tab: 'settings',
    target: [t('settings-backup')],
    body: [
      '<b>Backup &amp; restore</b> exports your rules and custom groups to a password-encrypted file and imports them back, on this Mac or another. <b>Reinstall daemon</b> fixes a version mismatch after an app update. Fallback DNS and timezone are here too.',
    ],
  },

  // ───────────────────────── Done
  {
    chapter: 'Done',
    id: 'finish',
    title: 'You’re set',
    body: [
      'A good first step: apply a group on <b>Rules</b>, or add an outbound on <b>Xray</b>, then watch it on <b>Map</b> and <b>Health</b>.',
      'From Terminal, <code>em-wall status</code>, <code>em-wall rules</code>, <code>em-wall group</code> and <code>em-wall health</code> show the same information, no sudo needed.',
    ],
    tip: 'You can open this tour again anytime from <b>Tour</b> at the bottom of the sidebar.',
  },
];

// ───────────────────────── Sections and runs

// The tour is these sections, in order. There is no single run through all
// of them: a run is one section, its last card shows every section's
// progress and offers the next one, so the order is kept without one long
// walk. The first launch plays Basics; the Tour menu starts any section.
export type TourSection = { id: string; title: string; blurb: string; icon: string; steps: TourStep[] };
const FINISH = STEPS.find((s) => s.id === 'finish')!;
// Icons are one stroke path on a 24×24 grid, like the sidebar's.
const SECTION_DEFS: { id: string; chapter: string; title: string; blurb: string; icon: string }[] = [
  { id: 'basics', chapter: 'Welcome', title: 'Basics', blurb: 'What em-wall does, finding your way, how to uninstall safely',
    icon: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM16.2 7.8l-2.1 6.3-6.3 2.1 2.1-6.3z' },
  { id: 'usecases', chapter: 'Use cases', title: 'Use cases', blurb: 'What em-wall is good for, and its limits today',
    icon: 'M9 18h6M10 22h4M12 2a7 7 0 0 0-4 12.7V16h8v-1.3A7 7 0 0 0 12 2z' },
  { id: 'rules', chapter: 'Rules', title: 'Rules', blurb: 'Patterns, actions, targets, groups, suggestions',
    icon: 'M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01' },
  { id: 'xray', chapter: 'Xray', title: 'Xray & proxies', blurb: 'Outbounds, subscriptions, masters, strategies, sets',
    icon: 'M12 2 3 7v10l9 5 9-5V7zM3 7l9 5 9-5M12 12v10' },
  { id: 'monitor', chapter: 'Monitor', title: 'Monitor', blurb: 'Map, usage, health, logs',
    icon: 'M22 12h-4l-3 9L9 3l-3 9H2' },
  { id: 'system', chapter: 'System', title: 'System', blurb: 'Network, DNS hijack, encrypted DNS, updates, backup',
    icon: 'M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6' },
];
export const SECTIONS: TourSection[] = SECTION_DEFS.map((d) => ({
  id: d.id, title: d.title, blurb: d.blurb, icon: d.icon, steps: STEPS.filter((s) => s.chapter === d.chapter),
}));
const ALL = SECTIONS.flatMap((s) => s.steps);

export type TourRun = {
  kind: 'section' | 'new';
  steps: TourStep[];
  // Section run: which section this is, and the one offered after it.
  section?: TourSection;
  next?: TourSection;
};

export function sectionRun(id: string): TourRun | null {
  const i = SECTIONS.findIndex((s) => s.id === id);
  if (i < 0) return null;
  const sec = SECTIONS[i];
  const next = SECTIONS[i + 1];
  // The closing card rides on the last section.
  return { kind: 'section', section: sec, next, steps: next ? sec.steps : [...sec.steps, FINISH] };
}
// By id: the section reaching the component is Vue's reactive proxy, which
// indexOf would never find in the raw list.
export function sectionPosition(sec: TourSection): number { return SECTIONS.findIndex((s) => s.id === sec.id) + 1; }

// Two separate records. `seen`: steps the user has been shown — drives the
// ✓ and progress per section. `known`: steps that existed when the user
// first got the tour, plus any shown since — what's NOT known is "new".
// Keeping them apart is what stops "you skipped five sections" from
// reading as "44 new things" on the next launch.
const SEEN_KEY = 'em-wall.tour.seen';
const KNOWN_KEY = 'em-wall.tour.known';

function readSet(key: string): Set<string> {
  try {
    const v = JSON.parse(localStorage.getItem(key) || '[]');
    return new Set(Array.isArray(v) ? v : []);
  } catch { return new Set(); }
}
function addTo(key: string, ids: string[]) {
  const set = readSet(key);
  for (const id of ids) if (!id.startsWith('__')) set.add(id);
  try { localStorage.setItem(key, JSON.stringify([...set])); } catch { /* ignore */ }
}
export function markSeen(steps: TourStep[]) {
  const ids = steps.map((s) => s.id);
  addTo(SEEN_KEY, ids);
  addTo(KNOWN_KEY, ids);
}
export function newSteps(): TourStep[] {
  const known = readSet(KNOWN_KEY);
  return ALL.filter((s) => !known.has(s.id));
}
export function sectionSeen(sec: TourSection): number {
  const seen = readSet(SEEN_KEY);
  return sec.steps.filter((s) => seen.has(s.id)).length;
}

// Steps added since the user's tour baseline, behind one intro card.
export function newRun(): TourRun | null {
  const fresh = newSteps();
  if (!fresh.length) return null;
  const intro: TourStep = {
    id: '__new',
    chapter: 'What’s new',
    title: fresh.length === 1 ? 'One new thing in em-wall' : `${fresh.length} new things in em-wall`,
    body: ['Since you last took the tour, em-wall has learned a few things. Here they are, one card each:'],
    terms: fresh.map((s) => ({ term: s.chapter, def: s.title })),
    tip: 'Every section of the tour is always in the <b>Tour</b> menu at the bottom of the sidebar.',
  };
  return { kind: 'new', steps: [intro, ...fresh] };
}

// First launch (no baseline yet): every current step becomes known — none
// of it is "new" to someone just starting — and the section picker opens.
// Later: What's new if a release added steps, else nothing.
export function autoRun(): TourRun | 'picker' | null {
  // Storage unavailable reads as "no baseline"; don't open a tour on
  // every launch because of it.
  try { localStorage.getItem(KNOWN_KEY); } catch { return null; }
  if (readSet(KNOWN_KEY).size === 0) {
    addTo(KNOWN_KEY, ALL.map((s) => s.id));
    return 'picker';
  }
  return newRun();
}
