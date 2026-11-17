import test from 'node:test';
import assert from 'node:assert/strict';
import { loadApp, settle } from '../lib/load-app.mjs';

test('portable onboarding uses a separate explicit code, supports Windows and keeps the code out of commands', async t => {
  const app = await loadApp({ origin: 'https://app.example.test' });
  t.after(app.close);
  app.router.post('/api/connectors/bind-codes', { code: 'synthetic-one-time-code', expiresAt: new Date(Date.now()+600000).toISOString() }, 201);
  app.evaluate('renderPortableConnectors()');
  assert.equal(app.router.calls.filter(c => c.path === '/api/connectors/bind-codes').length, 0);
  const platform=app.document.querySelector('#connectorPlatform');
  platform.value='windows/amd64';
  platform.dispatchEvent(new app.window.Event('change', {bubbles:true}));
  assert.match(app.document.querySelector('#connectorDownload').href,/os=windows&arch=amd64/);
  assert.equal(app.document.querySelector('#connectorDownload').download,'loopforge-connect.exe');
  assert.match(app.document.querySelector('#connectorCommand').textContent,/\.\\loopforge-connect\.exe bind --server https:\/\/app\.example\.test/);
  app.document.querySelector('#createConnectorCode').click();
  await settle(5);
  assert.equal(app.document.querySelector('#connectorCode').textContent,'synthetic-one-time-code');
  assert.doesNotMatch(app.document.querySelector('#connectorCommand').textContent,/synthetic-one-time-code/);
  assert.equal(app.document.querySelector('#copyConnectorCode').disabled,false);
  app.evaluate(`state.connectorCode.expiresAt='2000-01-01T00:00:00Z';renderPortableConnectors()`);
  assert.equal(app.document.querySelector('#connectorCode').textContent,'');
  assert.equal(app.document.querySelector('#copyConnectorCode').disabled,true);
  assert.match(app.document.querySelector('#connectorCodeExpiry').textContent,/已过期/);
});

test('portable onboarding blocks non-loopback plaintext transport and escapes remote device names', async t => {
  const app=await loadApp({origin:'http://app.example.test'});
  t.after(app.close);
  app.evaluate(`state.portableConnectors=[{device:{id:'device',name:'<img src=x onerror=alert(1)>',os:'windows',expiresAt:'2099-01-01T00:00:00Z'},agents:[{id:'agent',deviceId:'device',provider:'minimax-code'}]}];renderPortableConnectors()`);
  assert.equal(app.document.querySelector('#createConnectorCode').disabled,true);
  assert.equal(app.document.querySelector('#copyConnectorCommand').disabled,true);
  assert.equal(app.document.querySelector('#portableConnectorList img'),null);
  assert.match(app.document.querySelector('#portableConnectorList').textContent,/MiniMax Code/);
  assert.match(app.document.querySelector('#portableConnectorList').textContent,/已注册身份/);
  assert.doesNotMatch(app.document.querySelector('#portableConnectorList').textContent,/在线|执行中/);
});

test('portable identity revocation requires confirmation and uses scoped management route', async t => {
  const app=await loadApp({origin:'https://app.example.test'});
  t.after(app.close);
  app.router.post('/api/connectors/agents/agent-one/revoke',{});
  app.router.get('/api/connectors',[]);
  app.evaluate(`afConfirm=async()=>false`);
  await app.evaluate(`revokePortableConnector('agents','agent-one')`);
  assert.equal(app.router.calls.filter(c=>c.path.endsWith('/revoke')).length,0);
  app.evaluate(`afConfirm=async()=>true`);
  await app.evaluate(`revokePortableConnector('agents','agent-one')`);
  assert.equal(app.router.calls.filter(c=>c.path==='/api/connectors/agents/agent-one/revoke').length,1);
});
