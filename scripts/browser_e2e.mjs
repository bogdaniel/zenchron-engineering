// Dependency-free automated Chrome runner. Browser/backend cases use the real
// isolated Go service; one labeled presentation regression mocks authority reads.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, readFile, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';

const base = process.env.ZENCHRON_E2E_URL;
assert(base && /^http:\/\/127\.0\.0\.1:\d+$/.test(base), 'isolated loopback service required');
const chromePath = process.env.ZENCHRON_E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const artifacts = process.env.ZENCHRON_E2E_ARTIFACTS || '/private/tmp/zenchron-browser-evidence';
await mkdir(artifacts, { recursive: true });
const profile = await mkdtemp(path.join(tmpdir(), 'zenchron-chrome-'));
const browser = spawn(chromePath, ['--headless=new', '--remote-debugging-port=0', '--remote-debugging-address=127.0.0.1',
  '--no-first-run', '--no-default-browser-check', '--disable-background-networking', '--disable-sync',
  '--disable-component-update', '--user-data-dir=' + profile, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });
let chromeLog = '';
browser.stderr.on('data', chunk => { chromeLog = (chromeLog + chunk).slice(-10000); });
let cdp;
const results = [];
const deadline = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check, label, timeout = 12000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (await check()) return;
    await deadline(100);
  }
  throw new Error('timed out: ' + label);
}
class CDP {
  constructor(socket) {
    this.socket = socket;
    this.next = 0;
    this.pending = new Map();
    socket.addEventListener('message', event => {
      const message = JSON.parse(event.data);
      const waiting = this.pending.get(message.id);
      if (!waiting) return;
      clearTimeout(waiting.timer);
      this.pending.delete(message.id);
      if (message.error) waiting.reject(new Error(JSON.stringify(message.error)));
      else waiting.resolve(message.result);
    });
  }
  call(method, params = {}) {
    const id = ++this.next;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error('CDP timeout: ' + method)); }, 15000);
      this.pending.set(id, { resolve, reject, timer });
      this.socket.send(JSON.stringify({ id, method, params }));
    });
  }
  async evaluate(expression) {
    const result = await this.call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
    return result.result.value;
  }
  async navigate(route) {
    await this.call('Page.navigate', { url: base + route });
    await until(() => this.evaluate(`location.pathname === ${JSON.stringify(route.split('?')[0])} && document.readyState === 'complete'`), route);
  }
}
async function check(name, run) {
  try { await run(); results.push({ name, status: 'passed' }); console.log('PASS ' + name); }
  catch (error) { const diagnostic = cdp ? await cdp.evaluate(`[document.querySelector('#plan-preview-status')?.textContent, document.querySelector('#plan-approval pre')?.textContent, document.querySelector('#objective-status')?.textContent].filter(Boolean).join(' · ')`).catch(() => '') : ''; results.push({ name, status: 'failed', error: error.message, diagnostic }); console.log('FAIL ' + name + ': ' + error.message + ' ' + diagnostic); }
}
async function screenshot(name) {
  const { data } = await cdp.call('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  await writeFile(path.join(artifacts, name + '.png'), Buffer.from(data, 'base64'));
}
async function post(route, body, token = 'browser-test-token') {
  return cdp.evaluate(`(async () => { const r = await fetch(${JSON.stringify(route)}, {method:'POST', headers:{'Content-Type':'application/json','Authorization':${JSON.stringify('Bearer ' + token)}},body:JSON.stringify(${JSON.stringify(body)})}); return {status:r.status, body:await r.json()}; })()`);
}
try {
  let port;
  await until(async () => {
    try { port = (await readFile(path.join(profile, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; return Boolean(port); }
    catch { if (browser.exitCode !== null) throw new Error('Chrome exited: ' + chromeLog); return false; }
  }, 'isolated Chrome launch');
  const targets = await (await fetch('http://127.0.0.1:' + port + '/json/list')).json();
  const socket = new WebSocket(targets.find(target => target.type === 'page').webSocketDebuggerUrl);
  await once(socket, 'open');
  cdp = new CDP(socket);
  await cdp.call('Page.enable');
  await cdp.call('Runtime.enable');
  await cdp.call('Network.enable');
  await cdp.call('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false });
  await check('login rejects wrong token and keyboard submits correct token', async () => {
    await cdp.navigate('/overview');
    assert(await cdp.evaluate(`Boolean(document.querySelector('#login-form'))`));
    await cdp.evaluate(`document.querySelector('#token').value='wrong';document.querySelector('#login-form').requestSubmit()`);
    await until(() => cdp.evaluate(`!document.querySelector('#login-error').hidden`), 'wrong token refusal');
    await cdp.evaluate(`document.querySelector('#token').value='browser-test-token';document.querySelector('#token').focus()`);
    await cdp.call('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Enter', code: 'Enter', text: '\r', unmodifiedText: '\r', windowsVirtualKeyCode: 13 });
    await cdp.call('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13 });
    await until(() => cdp.evaluate(`!document.querySelector('#login-form')`), 'authenticated overview');
  });
  await check('product navigation has functioning accessible routes', async () => {
    for (const route of ['/overview', '/products', '/objectives', '/plans', '/workgraphs', '/runs', '/decisions', '/agents', '/settings']) {
      await cdp.navigate(route);
      const view = await cdp.evaluate(`({heading:document.querySelector('h1')?.textContent, broken:/render failed|not found/.test(document.body.innerText), navigation:Boolean(document.querySelector('nav')),login:Boolean(document.querySelector('#login-form'))})`);
      assert(view.heading && !view.broken && view.navigation && !view.login, route + ': ' + JSON.stringify(view));
    }
  });
  await check('overview surfaces controller health and authority queues', async () => {
    await cdp.navigate('/overview');
    assert(await cdp.evaluate(`Boolean(document.querySelector('#controller-health a[href="/settings"]'))`),
      'controller health details must be one click away');
    assert(await cdp.evaluate(`/connected|offline|unavailable/i.test(document.querySelector('#controller-health').textContent)`),
      'controller state must be visible outside collapsed diagnostics');
    const actionSummary = await cdp.evaluate(`document.querySelector('#action-required-summary').textContent`);
    for (const queue of ['plan approvals', 'WorkUnit decisions', 'publication authority']) {
      assert(actionSummary.includes(queue), queue + ' missing from Action Required summary');
    }
  });
  await check('agent configuration exposes profiles and template empty states', async () => {
    await cdp.navigate('/agents');
    await until(() => cdp.evaluate(`/installed profile/i.test(document.querySelector('#profiles-status')?.textContent || '')`), 'controller-owned profiles');
    await until(() => cdp.evaluate(`document.querySelector('#profiles-list').children.length > 0`), 'rendered profile list');
    const profileText = await cdp.evaluate(`document.querySelector('#profiles-list').textContent`);
    assert(profileText.includes('browser-implementer'), 'configured profile must be visible');
    assert(await cdp.evaluate(`/no templates configured/i.test(document.querySelector('#templates-status')?.textContent || '')`), 'empty template configuration must be explicit');
  });
  await check('desktop and mobile layout have no document overflow', async () => {
    for (const width of [1440, 390]) {
      await cdp.call('Emulation.setDeviceMetricsOverride', { width, height: 1000, deviceScaleFactor: 1, mobile: width < 500 });
      for (const route of ['/overview', '/objectives', '/decisions', '/workgraphs', '/runs']) {
        await cdp.navigate(route);
        assert(await cdp.evaluate(`document.documentElement.scrollWidth <= innerWidth + 1`), route + ' overflows at ' + width);
      }
      await cdp.navigate('/overview');
      await screenshot(width > 500 ? 'desktop-overview' : 'mobile-overview');
    }
    await cdp.call('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false });
  });
  await check('live refresh retains focused controls and honest freshness', async () => {
    await cdp.navigate('/runs');
    await cdp.evaluate(`window.focusedControl=document.querySelector('a[href="/runs/browser-live"]');focusedControl.focus();window.initialFreshness=document.querySelector('#freshness')?.textContent`);
    await until(() => cdp.evaluate(`/live/i.test(document.querySelector('#live-indicator')?.textContent || '')`), 'confirmed freshness');
    assert(await cdp.evaluate(`document.activeElement === focusedControl`), 'focused node was replaced');
  });
  await check('controller observation failure does not block workspace refresh', async () => {
    await cdp.call('Network.setBlockedURLs', { urls: [base + '/v1/controller*'] });
    try {
      await cdp.navigate('/overview');
      await until(() => cdp.evaluate(`/controller offline/i.test(document.querySelector('#live-indicator')?.textContent || '') && /updated/.test(document.querySelector('#freshness')?.textContent || '')`), 'workspace refresh with controller offline');
      assert(await cdp.evaluate(`Boolean(document.querySelector('#overview-live .meta')?.textContent)`), 'workspace content should remain rendered');
    } finally {
      await cdp.call('Network.setBlockedURLs', { urls: [] });
    }
  });
  let binding;
  await check('stale generation and permission refusals leave durable run unchanged', async () => {
    binding = await cdp.evaluate(`fetch('/v1/controller',{headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json()).then(r=>r.controller_binding)`);
    assert(binding);
    const forbidden = await post('/v1/runs/browser-run/stop', { controller_binding: binding }, 'wrong');
    assert.equal(forbidden.status, 401);
    const stale = await post('/v1/runs/browser-run/stop', { controller_binding: '0'.repeat(64) });
    assert.equal(stale.status, 409);
    const state = await cdp.evaluate(`fetch('/v1/runs/browser-run',{headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json())`);
    assert.equal(state.disposition, 'active');
  });
  await check('stop requires confirmation and duplicate submissions have one effect', async () => {
    await cdp.navigate('/runs/browser-run');
    await cdp.evaluate(`document.querySelector('[data-action="/v1/runs/browser-run/stop"]').click()`);
    assert(await cdp.evaluate(`document.querySelector('#action-dialog').open`));
    assert(await cdp.evaluate(`document.querySelector('#action-note-field').hidden`), 'stop must not offer a note its endpoint refuses');
    assert(await cdp.evaluate(`document.querySelector('#action-dialog').innerText.includes('browser-run')`));
    await cdp.evaluate(`document.querySelector('#action-note').value='must not be sent'`);
    await cdp.evaluate(`document.querySelector('#action-confirm').click();document.querySelector('#action-confirm').click()`);
    await until(() => cdp.evaluate(`/applied/.test(document.querySelector('#action-result')?.textContent || '')`), 'governed stop result');
    const duplicate = await post('/v1/runs/browser-run/stop', { controller_binding: binding });
    assert.equal(duplicate.status, 200);
    assert.equal(duplicate.body.outcome, 'already_applied');
    const events = await cdp.evaluate(`fetch('/v1/runs/browser-run/events',{headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json())`);
    assert.equal(events.events.filter(event => event.type === 'run.cancelled').length, 1);
  });
  await check('stale plan revision is refused without authorization', async () => {
    const plan = process.env.ZENCHRON_E2E_PLAN;
    await cdp.navigate('/plans/' + plan);
    const button = await cdp.evaluate(`(() => {const e=document.querySelector('[data-action$="/reject"]');return e ? {revision:Number(e.dataset.revision),digest:e.dataset.digest}:null})()`);
    assert(button);
    const response = await post('/v1/plans/' + plan + '/reject', { controller_binding: binding, revision: button.revision, digest: '0'.repeat(64) });
    assert.equal(response.status, 409);
    assert.equal(response.body.code, 'digest_mismatch');
    await screenshot('desktop-plan');
  });
  await check('objective workspace survives reload and preserves durable plan context', async () => {
    const plan = process.env.ZENCHRON_E2E_PLAN;
    await cdp.navigate('/objectives');
    assert(await cdp.evaluate(`Boolean(document.querySelector('a[href="/objectives/${plan}"]'))`));
    await cdp.navigate('/objectives/' + plan);
    for (let visit = 0; visit < 2; visit++) {
      assert(await cdp.evaluate(`Boolean(document.querySelector('a[href="/plans/${plan}?revision=1"]'))`));
      for (const section of ['summary', 'plan', 'execution', 'decisions', 'evidence', 'changes']) {
        assert(await cdp.evaluate(`Boolean(document.getElementById('${section}'))`), section);
      }
      if (visit === 0) {
        const previousTimeOrigin = await cdp.evaluate('performance.timeOrigin');
        await cdp.call('Page.reload');
        await until(() => cdp.evaluate(`performance.timeOrigin !== ${JSON.stringify(previousTimeOrigin)} && document.readyState === 'complete'`), 'durable objective after reload');
      }
    }
    await screenshot('desktop-objective');
    await cdp.call('Emulation.setDeviceMetricsOverride', { width: 390, height: 1000, deviceScaleFactor: 1, mobile: true });
    assert(await cdp.evaluate(`document.documentElement.scrollWidth <= innerWidth`), 'mobile objective overflow');
    await screenshot('mobile-objective');
    await cdp.call('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false });
  });
  await check('planning identity survives reload without invented plan approval', async () => {
    await cdp.navigate('/objectives');
    assert(await cdp.evaluate(`Boolean(document.querySelector('a[href="/objectives/browser-planning-only"]'))`));
    await cdp.navigate('/objectives/browser-planning-only');
    for (let visit = 0; visit < 2; visit++) {
      assert(await cdp.evaluate(`/No plan revision is recorded/.test(document.querySelector('#plan').textContent)`));
      assert(await cdp.evaluate(`!document.querySelector('#plan-approval') && !document.querySelector('[data-action$="/approve"]')`));
      if (visit === 0) {
        await cdp.call('Page.reload');
        await until(() => cdp.evaluate(`Boolean(document.querySelector('#objective-live'))`), 'planning identity reload');
      }
    }
  });
  await check('objective offers exact guided plan actions and refuses changed preview identity', async () => {
    const plan = process.env.ZENCHRON_E2E_PLAN;
    await cdp.navigate('/objectives/' + plan);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('[data-action$="/approve"]'))`), 'objective approval preview');
    assert(await cdp.evaluate(`Boolean(document.querySelector('[data-action$="/revise"]'))`));
    assert(await cdp.evaluate(`Boolean(document.querySelector('[data-action$="/reject"]'))`));
    assert(await cdp.evaluate(`/profile browser-implementer@/.test(document.querySelector('#plan-approval').textContent)`));
    await cdp.evaluate(`document.querySelector('#plan-approval').dataset.revision='0';document.querySelector('#action-dialog').showModal();document.querySelector('#action-dialog').close()`);
    await until(() => cdp.evaluate(`/plan changed/.test(document.querySelector('#plan-preview-status').textContent)`), 'changed preview refusal');
    assert(await cdp.evaluate(`!document.querySelector('#plan-approval [data-action]')`));
  });
  await check('objective-scoped decision resolves durably without bypass execution', async () => {
    const plan = process.env.ZENCHRON_E2E_PLAN;
    const decision = process.env.ZENCHRON_E2E_OBJECTIVE_DECISION;
    await cdp.navigate('/decisions');
    assert(await cdp.evaluate(`Boolean(document.querySelector('a[href="/objectives/${plan}?revision=1#decisions"]'))`), 'inbox must return to objective context');
    await cdp.navigate('/objectives/' + plan);
    await cdp.evaluate(`document.querySelector('a[href="#decisions"]').click()`);
    const selector = '[data-action="/v1/decisions/' + decision + '/resolve"][data-outcome-value="allow"]';
    assert(await cdp.evaluate(`Boolean(document.querySelector(${JSON.stringify(selector)}))`));
    const invalid = await post('/v1/decisions/' + decision + '/resolve', { controller_binding: binding, outcome_kind: 'allow_deny', outcome_value: 'invalid' });
    assert.equal(invalid.status, 400);
    assert.equal(invalid.body.error, "invalid_request");
    await cdp.evaluate(`document.querySelector(${JSON.stringify(selector)}).click()`);
    assert(await cdp.evaluate(`document.querySelector('#action-dialog').innerText.includes(${JSON.stringify(decision)})`));
    await cdp.evaluate(`document.querySelector('#action-confirm').click()`);
    await until(() => cdp.evaluate(`/applied/.test(document.querySelector('#action-result').textContent)`), 'objective decision resolution');
    const graph = await cdp.evaluate(`fetch('/v1/workgraphs/${process.env.ZENCHRON_E2E_OBJECTIVE_GRAPH}', {headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json())`);
    const unit = graph.units.find(unit => unit.unit_id === 'implementation');
    assert.equal(unit.hold, undefined);
    assert.equal(unit.run, undefined, 'resolving a hold must not create bypass execution');
    await cdp.navigate('/objectives/' + plan);
    assert(await cdp.evaluate(`!document.querySelector(${JSON.stringify(selector)})`), 'resolved request must disappear after reconnect');
  });
  await check('plan approval binds exact assignment and starts no work before confirmation', async () => {
    const plan = process.env.ZENCHRON_E2E_PLAN;
    await cdp.navigate('/plans/' + plan);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('[data-action$=\"/approve\"]'))`), 'approval assignment preview');
    await until(() => cdp.evaluate(`/profile browser-implementer@/.test(document.body.innerText)`), 'resolved AgentProfile in preview');
    const bindingData = await cdp.evaluate(`(() => {const e=document.querySelector('[data-action$=\"/approve\"]');return {revision:Number(e.dataset.revision),digest:e.dataset.digest,assignments_digest:e.dataset.assignmentsDigest}})()`);
    assert(bindingData.assignments_digest);
    assert(await cdp.evaluate(`!document.querySelector('a[href^=\"/runs/run-plan-\"]')`), 'unapproved plan has a stage run');
    await cdp.evaluate(`document.querySelector('[data-action$=\"/approve\"]').click()`);
    assert(await cdp.evaluate(`document.querySelector('#action-dialog').innerText.includes(${JSON.stringify(bindingData.assignments_digest)})`));
    await cdp.evaluate(`document.querySelector('#action-confirm').click()`);
    await until(() => cdp.evaluate(`/applied/.test(document.querySelector('#action-result')?.textContent || '')`), 'bound approval');
    const duplicate = await post('/v1/plans/' + plan + '/approve', {controller_binding:binding,...bindingData});
    assert.equal(duplicate.body.outcome, 'already_applied');
    const changed = await post('/v1/plans/' + plan + '/approve', {controller_binding:binding,...bindingData,assignments_digest:'0'.repeat(64)});
    assert.equal(changed.status, 409);
  });
  await check('browser objective intake reuses existing plan without duplicate planning', async () => {
    await cdp.navigate('/objectives');
    await until(() => cdp.evaluate(`!document.querySelector('#objective-submit').disabled`), 'eligible intake options');
    assert(await cdp.evaluate(`Array.from(document.querySelector('#objective-profile').options).some(option => option.value === 'browser-implementer-alt')`), 'installed profiles must be selectable');
    await cdp.evaluate(`document.querySelector('#objective-issue').value='41';document.querySelector('#objective-form').requestSubmit();document.querySelector('#objective-form').requestSubmit()`);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('#objective-status a[href^=\"/objectives/\"]'))`), 'idempotent objective plan result');
    assert(await cdp.evaluate(`document.querySelector('#objective-submit').disabled`), 'ambiguous intake must not silently retry');
    assert.equal(await cdp.evaluate(`document.querySelector('#objective-status a').getAttribute('href')`), '/objectives/' + process.env.ZENCHRON_E2E_PLAN);
    await screenshot('desktop-objective-intake');
  });
  await check('selected profile preference reaches a deterministic objective plan', async () => {
    await cdp.navigate('/objectives');
    await until(() => cdp.evaluate(`!document.querySelector('#objective-submit').disabled`), 'eligible intake options');
    await cdp.evaluate(`document.querySelector('#objective-issue').value='42';document.querySelector('#objective-profile').value='browser-implementer-alt';document.querySelector('#objective-deterministic').checked=true;document.querySelector('#objective-form').requestSubmit()`);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('#objective-status a[href^="/objectives/"]'))`), 'profile-bound plan proposal');
    const href = await cdp.evaluate(`document.querySelector('#objective-status a').getAttribute('href')`);
    await cdp.navigate(href);
    await until(() => cdp.evaluate(`/browser-implementer-alt@1/.test(document.body.innerText)`), 'selected profile assignment');
    assert(await cdp.evaluate(`/unapproved/i.test(document.body.innerText)`), 'new plan must remain subject to human approval');
  });
  await check('WorkGraph displays dependencies and preactivation authority hold', async () => {
    await cdp.navigate('/workgraphs/' + process.env.ZENCHRON_E2E_GRAPH);
    const text = await cdp.evaluate(`document.body.innerText`);
    assert(text.includes('implementation') && text.includes('integration') && /decision|hold/i.test(text));
    const graph = await cdp.evaluate(`({edges:document.querySelectorAll('.graph-edge').length,units:Array.from(document.querySelectorAll('.graph-node')).map(n=>({id:n.dataset.unitId,column:n.style.gridColumn}))})`);
    assert.equal(graph.edges, 1, 'the admitted dependency must have a visible arrow');
    assert(Number(graph.units.find(n => n.id === 'integration').column) > Number(graph.units.find(n => n.id === 'implementation').column), 'dependent work must render in a later column');
    assert.equal(graph.units.find(n => n.id === 'parallel-verification').column,
      graph.units.find(n => n.id === 'implementation').column, 'independent work must remain parallel');
    await cdp.evaluate(`document.querySelector('.graph-node[href="#unit-integration"]').click()`);
    assert.equal(await cdp.evaluate(`location.hash`), '#unit-integration', 'work unit links must navigate to their evidence detail');
    await cdp.evaluate(`window.__graphMapMutations=0;new MutationObserver(()=>window.__graphMapMutations++).observe(document.querySelector('.graph-map'),{childList:true,subtree:true,attributes:true});document.activeElement.blur()`);
    await until(() => cdp.evaluate(`window.__graphMapMutations > 0`), 'live WorkGraph map reconciliation');
    assert.equal(await cdp.evaluate(`document.querySelectorAll('.graph-edge').length`), 1, 'live refresh must redraw dependency arrows');
    await cdp.navigate('/workgraphs/' + process.env.ZENCHRON_E2E_GRAPH);
    await screenshot('desktop-workgraph');
    await cdp.call('Emulation.setDeviceMetricsOverride', {width:390,height:1000,deviceScaleFactor:1,mobile:true});
    assert(await cdp.evaluate(`document.documentElement.scrollWidth <= innerWidth + 1`));
    await screenshot('mobile-workgraph');
    await cdp.call('Emulation.setDeviceMetricsOverride', {width:1440,height:1000,deviceScaleFactor:1,mobile:false});
  });
  await check('run detail offers only the current pause transition', async () => {
    await cdp.navigate('/runs/browser-live');
    assert(await cdp.evaluate(`Boolean(document.querySelector('[data-action="/v1/runs/browser-live/pause"]'))`));
    assert(await cdp.evaluate(`!document.querySelector('[data-action="/v1/runs/browser-live/unpause"]')`));
    await cdp.evaluate(`document.querySelector('[data-action="/v1/runs/browser-live/pause"]').click();document.querySelector('#action-note').value='pause reason';document.querySelector('#action-confirm').click()`);
    await until(() => cdp.evaluate(`/applied/.test(document.querySelector('#action-result')?.textContent || '')`), 'pause request');
    await cdp.evaluate(`document.querySelector('#action-close').click()`);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('[data-action="/v1/runs/browser-live/unpause"]'))`), 'paused view');
    assert(await cdp.evaluate(`!document.querySelector('[data-action="/v1/runs/browser-live/pause"]')`));
    await cdp.evaluate(`document.querySelector('[data-action="/v1/runs/browser-live/unpause"]').click();document.querySelector('#action-confirm').click()`);
    await until(() => cdp.evaluate(`/applied/.test(document.querySelector('#action-result')?.textContent || '')`), 'unpause request');
    await cdp.evaluate(`document.querySelector('#action-close').click()`);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('[data-action="/v1/runs/browser-live/pause"]'))`), 'resumed view');
    const events = await cdp.evaluate(`fetch('/v1/runs/browser-live/events',{headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json())`);
    assert.equal(events.events.filter(event => event.type === 'run.paused').length, 1);
    assert.equal(events.events.filter(event => event.type === 'run.unpaused').length, 1);
  });
  await check('objective without publication gate stays incomplete and non-actionable', async () => {
    await cdp.navigate('/objectives/' + process.env.ZENCHRON_E2E_PLAN);
    assert(await cdp.evaluate(`/No outstanding human publication request/.test(document.querySelector('#changes').textContent)`));
    assert(await cdp.evaluate(`!document.querySelector('#changes [data-action$="/authorize"]')`));
    assert(await cdp.evaluate(`!document.querySelector('#changes a[href*="/pull/"]')`));
  });
  await check('runtime refusal never exposes publication authorization', async () => {
    await cdp.navigate('/runs/browser-run');
    await until(() => cdp.evaluate(`!(/loading publication authority/i.test(document.querySelector('#publication-actions')?.textContent || ''))`), 'publication authority observation');
    const authority = await cdp.evaluate(`fetch('/v1/runs/browser-run/authority',{headers:{Authorization:'Bearer browser-test-token'}}).then(async r=>({status:r.status,body:await r.json()}))`);
    assert.equal(authority.status, 409, JSON.stringify(authority));
    assert.equal(authority.body.error, 'runtime_refused');
    assert(await cdp.evaluate(`/publication unavailable \\(http 409\\)/i.test(document.querySelector('#publication-actions').textContent)`));
    assert(await cdp.evaluate(`!Array.from(document.querySelectorAll('#publication-actions button')).some(button => /authorize publication/i.test(button.textContent))`));
  });
  await check('presentation replaces publication authority for the same candidate', async () => {
    await cdp.navigate('/runs/browser-run');
    await until(() => cdp.evaluate(`/publication unavailable \\(http 409\\)/i.test(document.querySelector('#publication-actions').textContent)`), 'initial real authority refusal');
    await cdp.evaluate(`(() => {
      const original = window.fetch.bind(window);
      const requests = [
        {id:'publication-a',digest:'digest-a',requires:['human_approval'],contract:{id:'contract-a'},action:{target:'open PR'},candidate:{revision:1,tree:'tree-a'}},
        {id:'publication-b',digest:'digest-b',requires:['human_approval'],contract:{id:'contract-b'},action:{target:'open PR'},candidate:{revision:1,tree:'tree-a'}}
      ];
      window.__authorityReadCount = 0;
      window.fetch = function (input, init) {
        if (String(input).includes('/v1/runs/browser-run/authority')) {
          const request = requests[Math.min(window.__authorityReadCount++, requests.length - 1)];
          return Promise.resolve(new Response(JSON.stringify(request), {status:200,headers:{'Content-Type':'application/json'}}));
        }
        return original(input, init);
      };
    })()`);
    await until(() => cdp.evaluate(`Boolean(document.querySelector('#publication-actions [data-request-id="publication-a"]'))`), 'first authority request');
    await until(() => cdp.evaluate(`Boolean(document.querySelector('#publication-actions [data-request-id="publication-b"]'))`), 'replacement authority request', 12000);
    assert.equal(await cdp.evaluate(`document.querySelectorAll('#publication-actions [data-request-id="publication-a"]').length`), 0);
    assert.equal(await cdp.evaluate(`document.querySelector('#publication-actions [data-request-id="publication-b"]').dataset.digest`), 'digest-b');
  });
  await check('designated operator resolves exact WorkUnit hold through browser', async () => {
    await cdp.navigate('/decisions');
    const decision = process.env.ZENCHRON_E2E_DECISION;
    const selector = '[data-action=\"/v1/decisions/' + decision + '/resolve\"][data-outcome-value=\"allow\"]';
    assert(await cdp.evaluate(`Boolean(document.querySelector(${JSON.stringify(selector)}))`), 'preactivation hold absent from Action Required');
    await cdp.evaluate(`document.querySelector(${JSON.stringify(selector)}).click()`);
    assert(await cdp.evaluate(`document.querySelector('#action-dialog').innerText.includes(${JSON.stringify(decision)})`));
    await cdp.evaluate(`document.querySelector('#action-confirm').click()`);
    await until(() => cdp.evaluate(`/applied/.test(document.querySelector('#action-result')?.textContent || '')`), 'decision resolution');
    const graph = await cdp.evaluate(`fetch('/v1/workgraphs/' + ${JSON.stringify(process.env.ZENCHRON_E2E_GRAPH)}, {headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json())`);
    const implementation = graph.units.find(unit => unit.unit_id === 'implementation');
    assert.equal(implementation.state, 'ready', 'resolving the hold must advance the WorkGraph frontier');
    assert(graph.frontier.includes('implementation'), 'newly authorized unit must appear in the runnable frontier');
    assert(graph.frontier.includes('parallel-verification'), 'independent work must also be ready');
    assert.equal(implementation.hold, undefined, 'resolved hold must leave Action Required and the unit');
    const duplicate = await post('/v1/decisions/' + decision + '/resolve', {controller_binding:binding,outcome_kind:'allow_deny',outcome_value:'allow'});
    assert.equal(duplicate.body.outcome,'already_applied');
    const changedReason = await post('/v1/decisions/' + decision + '/resolve', {controller_binding:binding,outcome_kind:'allow_deny',outcome_value:'allow',note:'changed rationale'});
    assert.equal(changedReason.status,409);
    const conflicting = await post('/v1/decisions/' + decision + '/resolve', {controller_binding:binding,outcome_kind:'allow_deny',outcome_value:'deny'});
    assert.equal(conflicting.status,409);
  });
  await check('theme control is keyboard reachable and changes appearance', async () => {
    await cdp.navigate('/overview');
    const prior = await cdp.evaluate(`getComputedStyle(document.body).backgroundColor`);
    await cdp.evaluate(`document.querySelector('#theme-toggle').focus()`);
    await cdp.call('Input.dispatchKeyEvent', {type:'keyDown',key:'Enter',code:'Enter',text:'\r',windowsVirtualKeyCode:13});
    await cdp.call('Input.dispatchKeyEvent', {type:'keyUp',key:'Enter',code:'Enter',windowsVirtualKeyCode:13});
    assert.notEqual(await cdp.evaluate(`getComputedStyle(document.body).backgroundColor`), prior);
    await screenshot('desktop-dark-overview');
    await cdp.call('Emulation.setDeviceMetricsOverride', {width:390,height:1000,deviceScaleFactor:1,mobile:true});
    await screenshot('mobile-dark-overview');
    await cdp.call('Emulation.setDeviceMetricsOverride', {width:1440,height:1000,deviceScaleFactor:1,mobile:false});
  });
  await check('controller disconnect disables governed actions', async () => {
    console.log('CONTROL:disconnect');
    await until(async () => { const r = await cdp.evaluate(`fetch('/v1/controller',{headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json())`); return r.live_reachable === false; }, 'controller disconnect');
    await cdp.navigate('/runs/browser-live');
    assert(await cdp.evaluate(`!document.querySelector('[data-action="/v1/runs/browser-live/stop"]')`));
    const result = await post('/v1/runs/browser-live/stop', { controller_binding: binding });
    assert.equal(result.status, 409);
    await cdp.navigate('/objectives');
    await until(() => cdp.evaluate(`Boolean(document.querySelector('#workspace-options-retry'))`), 'controller configuration recovery action');
    assert(await cdp.evaluate(`/serve is running for this workspace with the same state directory/i.test(document.querySelector('#objective-status').textContent)`));
    console.log('CONTROL:reconnect');
    await until(async () => cdp.evaluate(`fetch('/v1/controller',{headers:{Authorization:'Bearer browser-test-token'}}).then(r=>r.json()).then(r=>r.live_reachable)`), 'controller reconnect');
    await until(() => cdp.evaluate(`/development controller is unattested/i.test(document.querySelector('#objective-status').textContent) && !document.querySelector('#workspace-options-retry')`), 'automatic controller configuration recovery');
    assert(await cdp.evaluate(`document.querySelector('#objective-submit').disabled`), 'an unattested controller must keep objective submission disabled');
  });
  await check('HTTP disconnect marks retained data stale', async () => {
    await cdp.navigate('/runs');
    console.log('CONTROL:server-disconnect');
    await until(() => cdp.evaluate(`/stale/i.test(document.querySelector('#live-indicator')?.textContent || '')`), 'stale indicator', 25000);
    assert(await cdp.evaluate(`Boolean(document.querySelector('a[href="/runs/browser-live"]'))`));
    await screenshot('disconnected-workspace');
  });
} finally {
  await writeFile(path.join(artifacts, 'results.json'), JSON.stringify({ base, results }, null, 2));
  if (cdp) cdp.socket.close();
  browser.kill('SIGTERM');
  await Promise.race([once(browser, 'exit'), deadline(3000)]);
  if (browser.exitCode === null) browser.kill('SIGKILL');
	await rm(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
}
if (results.some(result => result.status !== 'passed')) process.exitCode = 1;
process.exit(process.exitCode || 0);
