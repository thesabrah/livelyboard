// Load test: 500 concurrent users updating scores and reading ranks.
//
//   k6 run loadtests/scenarios/score_rank.js
//   k6 run -e BASE_URL=http://localhost:18090 -e READ_RATIO=9 loadtests/scenarios/score_rank.js
//
// Each VU is one user: every loop it sends one request (READ_RATIO reads per score update,
// picked at random; reads are half team rank, half top-10 leaderboard) and then sleeps 1s,
// so 500 users ~= 500 req/s.
// Note: VUs wait for each response, so if the server slows down they send less traffic
// (a closed model). Watch `http_reqs` per second: if it falls below ~VUs/s, the server is the limit.
import http from 'k6/http';
import { check, sleep } from 'k6';
// import { htmlReport } from 'https://raw.githubusercontent.com/benc-uk/k6-reporter/main/dist/bundle.js';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const USERS = Number(__ENV.USERS || 50); // accounts that own the teams (setup only)
const TEAMS = Number(__ENV.TEAMS || 10000);
const READ_RATIO = Number(__ENV.READ_RATIO || 4); // reads (rank or leaderboard) per score update

export const options = {
  setupTimeout: '5m',
  batchPerHost: 20,
  discardResponseBodies: true, // saves memory; setup opts back in per request
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'], // k6 omits p99 by default
  scenarios: {
    basic: {
      executor: 'constant-vus',
      vus: 20,
      duration: '30s',
    },
    load: {
      executor: 'ramping-vus',
      startTime: '30s', // after basic, so the 20-user baseline isn't mixed into the ramp
      startVUs: 0,
      stages: [
        { duration: '1m', target: 500 },
        { duration: '1m', target: 1000 },
        { duration: '1m', target: 5000 },
        { duration: '2m', target: 0 },
      ],
      gracefulRampDown: '7s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<500', 'p(99)<1000'], // 95% under 500ms, 99% under 1s
    http_req_failed: ['rate<0.01'], // less than 1% of requests should fail
    // Same limits per endpoint and per scenario, so the summary shows which one is slow.
    'http_req_duration{name:POST /teams/{id}/score}': ['p(95)<500', 'p(99)<1000'],
    'http_req_duration{name:GET /teams/{id}/rank}': ['p(95)<500', 'p(99)<1000'],
    'http_req_duration{name:GET /leaderboard}': ['p(95)<500', 'p(99)<1000'],
    'http_req_duration{scenario:basic}': ['p(95)<500', 'p(99)<1000'],
    'http_req_duration{scenario:load}': ['p(95)<500', 'p(99)<1000'],
  },
};

const json = { 'Content-Type': 'application/json' };

function batchOK(reqs, want, what) {
  const res = http.batch(reqs);
  res.forEach((r) => {
    if (r.status !== want) throw new Error(`setup ${what}: got ${r.status} ${r.body}`);
  });
  return res;
}

// Runs once before the test; not measured. Users are unique per run so the same DB can be reused.
export function setup() {
  const run = Date.now();
  const creds = [...Array(USERS).keys()].map((i) => JSON.stringify({ username: `lt${run}-${i}`, password: 'loadtest-password' }));
  const body = { responseType: 'text', headers: json };

  batchOK(creds.map((c) => ['POST', `${BASE}/auth/register`, c, body]), 201, 'register');
  const tokens = batchOK(creds.map((c) => ['POST', `${BASE}/auth/login`, c, body]), 200, 'login')
    .map((r) => r.json('access_token'));

  // Random starting scores spread the teams out, so rank lookups have real work to do.
  const teams = [];
  for (let i = 0; i < TEAMS; i += 500) {
    const reqs = [];
    for (let j = i; j < Math.min(i + 500, TEAMS); j++) {
      const u = j % USERS;
      reqs.push(['POST', `${BASE}/teams`, JSON.stringify({ name: `team-${j}`, score: Math.floor(Math.random() * 1e6) }),
        { responseType: 'text', headers: { ...json, Authorization: `Bearer ${tokens[u]}` } }]);
    }
    batchOK(reqs, 201, 'create team').forEach((r, k) => teams.push([r.json('id'), (i + k) % USERS]));
  }
  return { tokens, teams };
}

const pick = (arr) => arr[Math.floor(Math.random() * arr.length)];

export default (data) => {
  if (Math.random() < 1 / (1 + READ_RATIO)) scoreUpdate(data);
  else if (Math.random() < 0.5) getRank(data);
  else getTop();
  sleep(1); // think time: a real player doesn't send back-to-back requests
};

function scoreUpdate(data) {
  const [id, u] = pick(data.teams);
  const res = http.post(`${BASE}/teams/${id}/score`,
    JSON.stringify({ event_id: uuidv7(), delta: 1 + Math.floor(Math.random() * 100) }),
    { headers: { ...json, Authorization: `Bearer ${data.tokens[u]}` }, tags: { name: 'POST /teams/{id}/score' } });
  check(res, { 'score 200': (r) => r.status === 200 });
}

function getRank(data) {
  const [id] = pick(data.teams);
  const res = http.get(`${BASE}/teams/${id}/rank`, { tags: { name: 'GET /teams/{id}/rank' } });
  check(res, { 'rank 200': (r) => r.status === 200 });
}

function getTop() {
  const res = http.get(`${BASE}/leaderboard`, { tags: { name: 'GET /leaderboard' } });
  check(res, { 'leaderboard 200': (r) => r.status === 200 });
}

// k6's helper library only ships UUIDv4, and the server requires v7 (48-bit ms timestamp first).
// Math.random is fine here: these only need to be unique, not unguessable.
function uuidv7() {
  const b = new Array(16);
  const ms = Date.now();
  for (let i = 0; i < 6; i++) b[i] = Math.floor(ms / 2 ** (8 * (5 - i))) % 256;
  for (let i = 6; i < 16; i++) b[i] = Math.floor(Math.random() * 256);
  b[6] = (b[6] & 0x0f) | 0x70; // version 7
  b[8] = (b[8] & 0x3f) | 0x80; // RFC 9562 variant
  const h = b.map((x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// export function handleSummary(data) {
//   return {
//     "summary.html": htmlReport(data),
//   };
// }