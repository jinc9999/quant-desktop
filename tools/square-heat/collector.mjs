// 币安广场热度采集器（旁路研究工具，不参与交易）
//
// 原理：币安广场没有官方读取 API，必须携带浏览器真实计算出的指纹请求头
// （bnc-uuid / csrftoken / device-info 等）。本工具用 Playwright 打开一次
// 广场页面嗅探这些请求头，之后每隔 SQ_INTERVAL_SEC 秒通过页面内 fetch
// 拉取热帖 feed，解析币种与互动数据，追加写入 data/posts.jsonl，
// 并输出每轮热度快照到 data/heat_snapshots.jsonl。
//
// 运行：node collector.mjs            （常驻循环）
//       node collector.mjs --once     （只抓一轮后退出）
// 环境变量：SQ_INTERVAL_SEC（默认 600，即 10 分钟）

import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DATA_DIR = process.env.SQ_DATA_DIR || path.join(__dirname, 'data');
const INTERVAL_MS = (Number(process.env.SQ_INTERVAL_SEC) || 600) * 1000;
const PAGE_SIZE = 20;
const FEED_PATH = '/bapi/composite/v9/friendly/pgc/feed/feed-recommend/list';
const FEED_BODY = { pageIndex: 1, pageSize: PAGE_SIZE, scene: 'web-homepage', contentIds: [] };
const API_PATTERN = /\/bapi\/composite\//i;
const SNIFF_HEADERS = [
  'bnc-uuid',
  'bnc-time-zone',
  'csrftoken',
  'clienttype',
  'lang',
  'versioncode',
  'device-info',
  'fvideo-id',
  'fvideo-token',
];
const HEAT_WINDOW_MS = 12 * 3600 * 1000;   // 12 小时热度窗口
const HEAT_HALFLIFE_MS = 6 * 3600 * 1000;  // 时间衰减半衰期约 4.2 小时

fs.mkdirSync(DATA_DIR, { recursive: true });
const postsFile = path.join(DATA_DIR, 'posts.jsonl');
const heatFile = path.join(DATA_DIR, 'heat_snapshots.jsonl');
const logFile = path.join(DATA_DIR, 'collector.log');

let browser;
let page;
let sniffedHeaders = null;
const seenIds = new Set();

function log(...args) {
  const line = `[${new Date().toISOString()}] ${args.join(' ')}`;
  console.log(line);
  try { fs.appendFileSync(logFile, line + '\n'); } catch {}
}

function loadSeenIds() {
  if (!fs.existsSync(postsFile)) return;
  try {
    const lines = fs.readFileSync(postsFile, 'utf8').split('\n');
    for (const l of lines) {
      if (!l.trim()) continue;
      try { const p = JSON.parse(l); if (p.id) seenIds.add(p.id); } catch {}
    }
    log(`已加载历史去重集合，共 ${seenIds.size} 条`);
  } catch (e) {
    log('加载历史帖子失败（忽略，继续）:', e.message);
  }
}

async function openBrowser() {
  if (browser) {
    try { await browser.close(); } catch {}
  }
  browser = null;
  page = null;
  sniffedHeaders = null;
  browser = await chromium.launch({
    headless: true,
    args: [
      '--no-sandbox',
      '--disable-dev-shm-usage',
      '--disable-blink-features=AutomationControlled',
    ],
  });
  const context = await browser.newContext({
    userAgent:
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36',
    locale: 'en-US',
    viewport: { width: 1440, height: 900 },
    extraHTTPHeaders: { 'accept-language': 'en-US,en;q=0.9' },
  });
  page = await context.newPage();
  await sniff();
}

// 反爬挑战是间歇性的：嗅探失败就换全新浏览器会话重试
async function openBrowserWithRetry(maxAttempts = 5, delayMs = 20000) {
  for (let i = 1; i <= maxAttempts; i++) {
    try {
      await openBrowser();
      return;
    } catch (e) {
      log(`打开浏览器会话第 ${i}/${maxAttempts} 次失败: ${e.message}`);
      if (i < maxAttempts) await new Promise((r) => setTimeout(r, delayMs));
    }
  }
  throw new Error('连续多次无法建立可用浏览器会话，请检查网络/代理或稍后再试');
}

// 打开广场页面，截获 feed 请求的真实请求头（含浏览器指纹）
async function sniff() {
  let captured = null;
  const handler = (req) => {
    if (!API_PATTERN.test(req.url())) return;
    const h = req.headers();
    if (!h['device-info']) return;
    captured = h;
  };
  page.on('request', handler);
  try {
    await page.goto('https://www.binance.com/en/square', {
      waitUntil: 'domcontentloaded',
      timeout: 90000,
    });
    const deadline = Date.now() + 15000;
    while (!captured && Date.now() < deadline) {
      await page.waitForTimeout(200);
    }
  } finally {
    page.off('request', handler);
  }
  if (!captured) {
    throw new Error('未能嗅探到 feed 请求头（页面可能被反爬拦截或网络异常）');
  }
  const keep = {};
  for (const k of SNIFF_HEADERS) {
    if (captured[k]) keep[k] = captured[k];
  }
  if (!keep['device-info']) {
    throw new Error('嗅探到的请求头缺少 device-info（反爬可能已更新）');
  }
  sniffedHeaders = keep;
  log('嗅探到请求头:', Object.keys(keep).join(','));
}

// 在页面内发 POST（浏览器自动带 cookie 与系统代理），返回 vos 列表
async function fetchFeed() {
  const res = await page.evaluate(async ({ url, headers, body }) => {
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...headers },
      body: JSON.stringify(body),
    });
    return { ok: r.ok, status: r.status, json: await r.json() };
  }, { url: FEED_PATH, headers: sniffedHeaders, body: FEED_BODY });

  if (!res.ok) throw new Error(`feed HTTP ${res.status}`);
  if (!res.json || !res.json.success) {
    throw new Error(`feed 业务错误: ${(res.json && res.json.message) || 'unknown'}`);
  }
  const vos = (res.json.data && res.json.data.vos) || [];
  return vos;
}

function pushCoins(coins, v) {
  if (v == null) return;
  if (typeof v === 'string') {
    const s = v.toUpperCase().trim();
    if (/^[A-Z0-9]{2,20}$/.test(s)) coins.add(s);
  } else if (Array.isArray(v)) {
    for (const x of v) pushCoins(coins, x);
  } else if (typeof v === 'object') {
    pushCoins(coins, v.symbol || v.baseAsset || v.asset);
  }
}

function extractCoins(item) {
  const coins = new Set();
  pushCoins(coins, item.tradingPairs);
  pushCoins(coins, item.coinPairList);
  pushCoins(coins, item.tradingPairsV2);
  pushCoins(coins, item.userInputTradingPairs);
  if (Array.isArray(item.hashtagList)) {
    for (const h of item.hashtagList) {
      const name = (typeof h === 'string' ? h : (h.name || h.title || ''));
      const m = name.match(/[A-Za-z0-9]{2,12}/);
      if (m) coins.add(m[0].toUpperCase());
    }
  }
  return [...coins];
}

function toPost(item) {
  const coins = extractCoins(item);
  const num = (v) => (typeof v === 'number' && isFinite(v) ? v : 0);
  return {
    id: item.id,
    fetchedAt: Date.now(),
    publishedAt: num(item.date) ? item.date * 1000 : null,
    cardType: item.cardType || '',
    contentType: item.contentType || '',
    author: item.authorName || item.username || '',
    authorVerificationType: item.authorVerificationType ?? null,
    authorRole: item.authorRole ?? null,
    title: (item.title || '').slice(0, 300),
    content: (item.content || '').slice(0, 500),
    coins,
    viewCount: num(item.viewCount),
    likeCount: num(item.likeCount),
    commentCount: num(item.commentCount),
    shareCount: num(item.shareCount),
    replyCount: num(item.replyCount),
    totalReactionCount: num(item.totalReactionCount),
  };
}

function computeHeat(recentPosts) {
  const now = Date.now();
  const map = new Map();
  for (const p of recentPosts) {
    const age = now - p.fetchedAt;
    if (age < 0 || age > HEAT_WINDOW_MS) continue;
    const decay = Math.exp(-age / HEAT_HALFLIFE_MS);
    const w = 1
      + Math.log1p(p.viewCount || 0) * 0.3
      + Math.log1p(p.commentCount || 0) * 1.5
      + Math.log1p(p.likeCount || 0) * 0.6
      + Math.log1p(p.shareCount || 0) * 1.0
      + Math.log1p(p.replyCount || 0) * 1.0;
    for (const c of p.coins || []) {
      const key = c.replace(/USDT$/, '');
      if (!/^[A-Z0-9]{2,12}$/.test(key)) continue;
      const e = map.get(key) || {
        coin: key,
        heat: 0,
        mentions: 0,
        posts: 0,
        views: 0,
        comments: 0,
        lastSeen: 0,
      };
      e.heat += w * decay;
      e.mentions += 1;
      e.posts += 1;
      e.views += p.viewCount || 0;
      e.comments += p.commentCount || 0;
      e.lastSeen = Math.max(e.lastSeen, p.fetchedAt);
      map.set(key, e);
    }
  }
  return [...map.values()].sort((a, b) => b.heat - a.heat).slice(0, 40);
}

async function runOnce() {
  const vos = await fetchFeed();
  let added = 0;
  for (const item of vos) {
    if (!item || !item.id || seenIds.has(item.id)) continue;
    seenIds.add(item.id);
    fs.appendFileSync(postsFile, JSON.stringify(toPost(item)) + '\n');
    added++;
  }

  const recent = [];
  if (fs.existsSync(postsFile)) {
    const lines = fs.readFileSync(postsFile, 'utf8').split('\n');
    const cutoff = Date.now() - HEAT_WINDOW_MS;
    for (const l of lines) {
      if (!l.trim()) continue;
      try {
        const p = JSON.parse(l);
        if (p.fetchedAt >= cutoff) recent.push(p);
      } catch {}
    }
  }
  const heat = computeHeat(recent);
  fs.appendFileSync(heatFile, JSON.stringify({ ts: Date.now(), top: heat }) + '\n');
  const top3 = heat.slice(0, 3).map((h) => `${h.coin}=${h.heat.toFixed(1)}`).join(' ');
  log(`抓取 ${vos.length} 条 | 新增 ${added} | 累计 ${seenIds.size} | 12h热度TOP3: ${top3}`);
}

async function main() {
  const once = process.argv.includes('--once');
  loadSeenIds();
  await openBrowserWithRetry();

  const loop = async () => {
    try {
      await runOnce();
    } catch (e) {
      log('本轮失败:', e.message);
      // 会话可能过期/被反爬拦截：换全新浏览器会话后重试一次
      try {
        await openBrowser();
        await runOnce();
      } catch (e2) {
        log('重试仍失败:', e2.message);
      }
    }
  };

  await loop();
  if (once) {
    try { await browser.close(); } catch {}
    log('--once 模式完成，退出');
    process.exit(0);
  }

  log(`常驻模式启动，每 ${INTERVAL_MS / 1000} 秒采集一轮`);
  const timer = setInterval(loop, INTERVAL_MS);
  const shutdown = async () => {
    clearInterval(timer);
    try { await browser.close(); } catch {}
    log('已停止');
    process.exit(0);
  };
  process.on('SIGINT', shutdown);
  process.on('SIGTERM', shutdown);
}

main().catch((e) => {
  log('致命错误:', e.message);
  process.exit(1);
});
