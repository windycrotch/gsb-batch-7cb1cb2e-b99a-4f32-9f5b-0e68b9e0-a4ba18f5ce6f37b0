const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);
for (const [key, value] of new URLSearchParams(location.hash.slice(1))) params.set(key, value);
const GATHERING_TIMEOUT_MS = 3000;

const invitation = params.has('room') ? { room: params.get('room'), token: params.get('t') ?? '' } : null;

const state = { ws: null, pc: null, mic: null, me: null, peer: null, leaving: false, invite: null };

function setState(name) {
  document.body.dataset.state = name;
}

function showError(text) {
  $('error').textContent = text;
  $('error').hidden = !text;
}

function setStatus(text) {
  $('status').textContent = text;
}

function setPeer(peer) {
  state.peer = peer;
  $('peer-label').textContent = peer
    ? `Talking with ${peer.name} (${peer.lang})`
    : 'Waiting for someone to join…';
  $('invite').hidden = Boolean(peer) || !state.invite;
}

function inviteLink({ room, token }) {
  const query = new URLSearchParams({ room });
  if (token) query.set('t', token);
  return `${location.origin}/#${query}`;
}

function showInvite(grant) {
  state.invite = grant;
  $('invite-link').value = inviteLink(grant);
  $('copy').textContent = 'Copy';
  const until = grant.expires
    ? ` It works until ${new Date(grant.expires).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}.`
    : '';
  $('invite-text').textContent = `Send this link to the person you want to talk to.${until}`;
}

async function createRoom() {
  const headers = {};
  const key = $('key').value;
  if (key) headers.Authorization = `Bearer ${key}`;
  const response = await fetch('/rooms', { method: 'POST', headers });
  if (!response.ok) throw new Error((await response.text()).trim() || `could not start a call (${response.status})`);
  return response.json();
}

function send(message) {
  if (state.ws?.readyState === WebSocket.OPEN) state.ws.send(JSON.stringify(message));
}

function addCaption({ from, kind, utterance, text, final = true }) {
  const id = `caption-${from}-${utterance}`;
  let item = document.getElementById(id);
  if (!item) {
    const mine = from === state.me.id;
    item = document.createElement('li');
    item.id = id;
    item.className = mine ? 'mine' : 'theirs';
    for (const part of ['who', 'said', 'heard']) {
      const line = document.createElement(part === 'who' ? 'div' : 'p');
      line.className = part;
      item.append(line);
    }
    item.querySelector('.who').textContent = mine ? 'You' : state.peer?.name ?? 'Guest';
    $('captions').append(item);
  }
  const settled = `${kind}Final`;
  if (!final && item.dataset[settled]) return;
  const mine = item.classList.contains('mine');
  const primary = mine ? 'source' : 'translation';
  item.querySelector(kind === primary ? '.said' : '.heard').textContent = text;
  item.dataset[kind] = text;
  if (final) item.dataset[settled] = '1';
  item.classList.toggle('draft', !(item.dataset.sourceFinal && item.dataset.translationFinal));
  item.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
}

function gatheringComplete(pc) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, GATHERING_TIMEOUT_MS);
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') {
        clearTimeout(timer);
        resolve();
      }
    });
  });
}

async function connect(iceServers) {
  const pc = new RTCPeerConnection({ iceServers });
  state.pc = pc;
  for (const track of state.mic.getTracks()) pc.addTrack(track, state.mic);
  pc.addEventListener('track', (event) => {
    $('remote').srcObject = event.streams[0] ?? new MediaStream([event.track]);
  });
  pc.addEventListener('connectionstatechange', () => {
    setState(pc.connectionState === 'connected' ? 'connected' : 'connecting');
    if (pc.connectionState === 'failed') showError('The media connection failed.');
  });
  await pc.setLocalDescription(await pc.createOffer());
  await gatheringComplete(pc);
  send({ type: 'offer', sdp: pc.localDescription.sdp });
}

async function handle(message) {
  switch (message.type) {
    case 'joined':
      state.me = message;
      showSameRoom(message.sameRoom);
      showHeadphones(message.headphones);
      setPeer(message.peer ?? null);
      setStatus(message.connected ? `Translation engine: ${message.backend}` : 'Translation engine offline');
      await connect(message.iceServers ?? []);
      break;
    case 'answer':
      await state.pc.setRemoteDescription({ type: 'answer', sdp: message.sdp });
      break;
    case 'peer-joined':
      showError('');
      setPeer(message.peer);
      break;
    case 'same-room':
      showSameRoom(message.sameRoom);
      break;
    case 'headphones':
      showHeadphones(message.headphones);
      break;
    case 'peer-left':
      showError('');
      setPeer(null);
      break;
    case 'caption':
      showError('');
      addCaption(message);
      break;
    case 'media':
      setStatus(message.connected ? `Translation engine: ${message.backend}` : 'Translation engine offline');
      break;
    case 'error':
      showError(message.message);
      break;
  }
}

function hangUp() {
  state.leaving = true;
  send({ type: 'leave' });
  state.ws?.close();
  state.pc?.close();
  state.mic?.getTracks().forEach((track) => track.stop());
  Object.assign(state, { ws: null, pc: null, mic: null, me: null, peer: null });
  $('captions').replaceChildren();
  $('call').hidden = true;
  $('join').hidden = false;
  setState('idle');
}

async function join({ room, token }, lang, name) {
  showError('');
  state.leaving = false;
  setState('connecting');
  const processing = params.get('raw') !== '1';
  try {
    state.mic = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: processing, noiseSuppression: processing, autoGainControl: processing },
    });
  } catch (error) {
    setState('idle');
    showError(`Microphone unavailable: ${error.message}`);
    return;
  }

  $('join').hidden = true;
  $('call').hidden = false;
  $('room-label').textContent = token ? 'Private call' : room;
  setPeer(null);

  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${scheme}://${location.host}/ws`);
  state.ws = ws;
  ws.addEventListener('open', () => send({ type: 'join', room, lang, name, token }));
  ws.addEventListener('message', (event) => {
    handle(JSON.parse(event.data)).catch((error) => showError(error.message));
  });
  ws.addEventListener('close', (event) => {
    if (state.leaving || state.ws !== ws) return;
    const reason = event.reason || 'The connection to the server was lost.';
    hangUp();
    showError(reason);
  });
}

$('join').addEventListener('submit', async (event) => {
  event.preventDefault();
  showError('');
  const lang = $('lang').value;
  const name = $('name').value.trim();
  if (invitation) {
    state.invite = null;
    await join(invitation, lang, name);
    return;
  }
  try {
    showInvite(await createRoom());
  } catch (error) {
    showError(error.message);
    return;
  }
  await join(state.invite, lang, name);
});

$('copy').addEventListener('click', async () => {
  try {
    await navigator.clipboard.writeText($('invite-link').value);
    $('copy').textContent = 'Copied';
  } catch {
    $('invite-link').select();
  }
});

if (navigator.share) {
  $('share').hidden = false;
  $('share').addEventListener('click', () => {
    navigator.share({ title: 'ambivox', text: 'Join my call', url: $('invite-link').value }).catch(() => {});
  });
}

$('leave').addEventListener('click', hangUp);

function showSameRoom(on) {
  $('same-room').setAttribute('aria-pressed', String(Boolean(on)));
  const automatic = params.get('raw') !== '1' && !on;
  state.mic?.getAudioTracks().forEach((track) => {
    track.applyConstraints({ autoGainControl: automatic }).catch(() => {});
  });
}

$('same-room').addEventListener('click', () => {
  send({ type: 'same-room', sameRoom: $('same-room').getAttribute('aria-pressed') !== 'true' });
});

function showHeadphones(on) {
  $('headphones').setAttribute('aria-pressed', String(Boolean(on)));
}

$('headphones').addEventListener('click', () => {
  send({ type: 'headphones', headphones: $('headphones').getAttribute('aria-pressed') !== 'true' });
});

$('mute').addEventListener('click', () => {
  const muted = $('mute').getAttribute('aria-pressed') !== 'true';
  state.mic?.getAudioTracks().forEach((track) => { track.enabled = !muted; });
  $('mute').setAttribute('aria-pressed', String(muted));
  $('mute').textContent = muted ? 'Unmute' : 'Mute';
});

for (const field of ['name', 'lang']) {
  if (params.has(field)) $(field).value = params.get(field);
}
if (invitation) {
  $('invited').hidden = false;
  $('go').textContent = 'Join the call';
}
fetch('/config')
  .then((response) => response.json())
  .then((config) => { $('key-field').hidden = !config.accessKey || Boolean(invitation); })
  .catch(() => {});
if (params.get('auto') === '1') $('join').requestSubmit();
