// "Workday": 400 users at once, each browsing folders of their own tree —
// open a folder, sometimes scroll on to further pages, think, repeat.
// Thresholds from the PRD: listing p95 < 300 ms and not a single 5xx.
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';
import { listPage, users } from './common.js';

const serverErrors = new Counter('server_errors');
const peak = Number(__ENV.VUS || 400);
const hold = __ENV.HOLD || '5m';

export const options = {
  scenarios: {
    workday: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '1m', target: peak },
        { duration: hold, target: peak },
        { duration: '30s', target: 0 },
      ],
      gracefulRampDown: '10s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<300'],
    server_errors: ['count==0'],
    checks: ['rate>0.99'],
  },
};

// Most people browse by name; the other orders get a share.
const sorts = ['name', 'name', 'name', 'name', 'mtime', 'mtime', 'size'];

function pick(list) {
  return list[Math.floor(Math.random() * list.length)];
}

export default function () {
  // Each VU is one user, so 400 VUs browse 400 different trees.
  const user = users[(__VU - 1) % users.length];
  const dir = pick(user.dirs);
  const sort = pick(sorts);
  // Usual direction: A→Z by name, newest or largest first otherwise.
  const usual = sort === 'name' ? 'asc' : 'desc';
  const order = Math.random() < 0.8 ? usual : usual === 'asc' ? 'desc' : 'asc';
  let cursor = null;
  // After the first page, one time in ten the user scrolls on — up to
  // three more pages.
  for (let page = 0; page < 4; page++) {
    const res = listPage(user, dir, { sort, order, cursor });
    if (res.status >= 500) serverErrors.add(1);
    if (!check(res, { 'status 200': (r) => r.status === 200 })) break;
    cursor = res.json('nextCursor');
    if (!cursor || Math.random() >= 0.1) break;
  }
  sleep(1 + Math.random() * 2);
}
