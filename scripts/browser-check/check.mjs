// Starts a call from the web client in headless Chrome over the self-signed
// HTTPS setup, opens the invitation in a second page, feeds both microphones
// with a phrase spoken by the simulated backend, and checks the captions.
import { spawn, execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { get } from 'node:https';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import puppeteer from 'puppeteer-core';

const CHROME_CANDIDATES = [
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];
const PORT = process.env.PORT ?? '8097';
const TIMEOUT_MS = 40_000;
const ACCESS_KEY = 'browser-check';
const ORIGIN = `https://127.0.0.1:${PORT}`;

const required = (name) => {
  const value = process.env[name];
  if (!value || !existsSync(value)) throw new Error(`${name} must point to a built binary (run via \`make browser-check\`)`);
  return value;
};
const mediaBin = required('MEDIA_BIN');
const serverBin = required('SERVER_BIN');
const chrome = CHROME_CANDIDATES.find((path) => path && existsSync(path));
if (!chrome) throw new Error('Chrome not found; set CHROME to its executable');

const dir = mkdtempSync(join(tmpdir(), 'ambivox-browser-'));
const socket = join(dir, 'media.sock');
const wav = join(dir, 'hello.wav');
execFileSync(mediaBin, ['synth', '--text', 'hello world', '--lang', 'en', '--wav', wav]);

const children = [
  spawn(mediaBin, ['serve', '--socket', socket], { stdio: 'ignore' }),
  spawn(
    serverBin,
    [
      '-listen', `127.0.0.1:${PORT}`, '-media-socket', socket, '-tls-self-signed', '-data-dir', dir, '-udp-port', PORT,
      // Both fake microphones repeat their phrase without ever pausing.
      '-echo-guard=false',
    ],
    { stdio: 'ignore', env: { ...process.env, AMBIVOX_ACCESS_KEY: ACCESS_KEY } },
  ),
];
let browser;
const cleanup = async () => {
  await browser?.close().catch(() => {});
  children.forEach((child) => child.kill());
  rmSync(dir, { recursive: true, force: true });
};

const fetchText = (path) =>
  new Promise((resolve) => {
    get(`${ORIGIN}${path}`, { rejectUnauthorized: false }, (response) => {
      let body = '';
      response.on('data', (chunk) => { body += chunk; });
      response.on('end', () => resolve({ status: response.statusCode, body }));
    }).on('error', () => resolve({ status: 0, body: '' }));
  });

const ready = async () => (await fetchText('/readyz')).status === 200;

async function waitForServer() {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (await ready()) return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error('the server never became ready');
}

try {
  await waitForServer();
  browser = await puppeteer.launch({
    executablePath: chrome,
    headless: true,
    args: [
      '--no-sandbox',
      '--ignore-certificate-errors',
      '--use-fake-ui-for-media-stream',
      '--use-fake-device-for-media-stream',
      `--use-file-for-fake-audio-capture=${wav}`,
      '--autoplay-policy=no-user-gesture-required',
    ],
  });

  const open = async (url, name) => {
    const page = await browser.newPage();
    page.on('pageerror', (error) => console.error(`[${name}] ${error.message}`));
    await page.goto(url);
    return page;
  };

  const alice = await open(`${ORIGIN}/?lang=en&name=Alice&raw=1`, 'Alice');
  await alice.waitForSelector('#key', { visible: true });
  await alice.type('#key', 'wrong');
  await alice.click('#go');
  await alice.waitForFunction(() => document.getElementById('error').textContent.includes('access key'), { polling: 200 });
  await alice.evaluate(() => { document.getElementById('key').value = ''; });
  await alice.type('#key', ACCESS_KEY);
  await alice.click('#go');
  const link = await alice
    .waitForFunction(() => document.getElementById('invite-link').value, { timeout: TIMEOUT_MS, polling: 200 })
    .then((handle) => handle.jsonValue());

  if (!link.startsWith(`${ORIGIN}/#room=`)) throw new Error(`the invitation should travel in the URL fragment: ${link}`);

  const forged = await open(`${link.replace(/t=[^.]+/, 't=zz')}&lang=es&name=Eve&auto=1&raw=1`, 'Eve');
  await forged.waitForFunction(() => document.getElementById('error').textContent.includes('invitation'), {
    timeout: TIMEOUT_MS,
    polling: 200,
  });
  await forged.close();

  const bob = await open(`${link}&lang=fr&name=Bob&auto=1&raw=1`, 'Bob');

  const captionFrom = (page, who, said) =>
    page
      .waitForFunction(
        (speaker, source) => {
          const item = [...document.querySelectorAll('#captions li.theirs')].find(
            (li) =>
              li.dataset.translationFinal &&
              li.dataset.source === source &&
              li.querySelector('.who').textContent === speaker,
          );
          return item ? { source: item.dataset.source, translation: item.dataset.translation } : false;
        },
        { timeout: TIMEOUT_MS, polling: 200 },
        who,
        said,
      )
      .then((handle) => handle.jsonValue());

  const heardByBob = await captionFrom(bob, 'Alice', 'hello world');
  const heardByAlice = await captionFrom(alice, 'Bob', 'hello world');
  for (const page of [alice, bob]) {
    const formShown = await page.evaluate(() => getComputedStyle(document.getElementById('join')).display !== 'none');
    if (formShown) throw new Error('the join form is still visible during the call');
  }
  if (await alice.$eval('#invite', (box) => !box.hidden)) throw new Error('the invitation is still shown once Bob joined');
  const expect = (actual, wanted, label) => {
    if (JSON.stringify(actual) !== JSON.stringify(wanted)) {
      throw new Error(`${label}: got ${JSON.stringify(actual)}, want ${JSON.stringify(wanted)}`);
    }
  };
  expect(heardByBob, { source: 'hello world', translation: 'bonjour monde' }, 'Bob hearing Alice');
  expect(heardByAlice, { source: 'hello world', translation: 'hello world' }, 'Alice hearing Bob');

  for (const id of ['headphones', 'same-room']) {
    await alice.evaluate((id) => document.getElementById(id).click(), id);
    for (const page of [alice, bob]) {
      await page.waitForFunction((id) => document.getElementById(id).getAttribute('aria-pressed') === 'true', {
        timeout: TIMEOUT_MS,
        polling: 200,
      }, id);
    }
  }
  const muted = /^ambivox_rtp_packets_same_room_muted_total (\d+)$/m;
  const deadline = Date.now() + TIMEOUT_MS;
  for (;;) {
    const match = muted.exec((await fetchText('/metrics')).body);
    if (match && Number(match[1]) > 0) break;
    if (Date.now() > deadline) throw new Error('same room mode never silenced a microphone: Chrome sent no audio levels');
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  console.log('browser check passed: invited call over HTTPS translated end to end in Chrome');
} catch (error) {
  console.error(`browser check failed: ${error.message}`);
  process.exitCode = 1;
} finally {
  await cleanup();
}
