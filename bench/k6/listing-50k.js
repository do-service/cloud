// G2: listing a folder of 50 000 items, server side p95 < 200 ms. Each VU
// pages through a big folder from start to end in one sort order; six VUs
// cover name/mtime/size × asc/desc at once.
import { check, fail } from 'k6';
import { listPage, users } from './common.js';

const combos = ['name', 'mtime', 'size'].flatMap((sort) => ['asc', 'desc'].map((order) => ({ sort, order })));
const bigUsers = users.filter((u) => u.bigDirId);
const passes = Number(__ENV.PASSES || 1);
const bigDirSize = Number(__ENV.BIG_DIR_SIZE || 50000);

export const options = {
  scenarios: {
    pages: { executor: 'per-vu-iterations', vus: combos.length, iterations: passes, maxDuration: '10m' },
  },
  thresholds: {
    // The first page is what G2 measures; later pages must hold the same
    // bound, or scrolling a big folder stalls.
    'http_req_duration{page:first}': ['p(95)<200'],
    http_req_duration: ['p(95)<200'],
    http_req_failed: ['rate==0'],
    checks: ['rate==1'],
  },
};

export function setup() {
  if (bigUsers.length === 0) fail('no user in the tokens file has a big folder (benchgen -big-dirs)');
}

export default function () {
  const { sort, order } = combos[(__VU - 1) % combos.length];
  const user = bigUsers[(__VU - 1) % bigUsers.length];
  let cursor = null;
  let items = 0;
  do {
    const res = listPage(user, user.bigDirId, { sort, order, cursor, tags: { page: cursor ? 'next' : 'first', sort, order } });
    if (!check(res, { 'status 200': (r) => r.status === 200 })) return;
    const body = res.json();
    items += body.items.length;
    cursor = body.nextCursor;
  } while (cursor);
  // Keyset paging must neither skip nor repeat an item.
  check(items, { 'every item listed once': (n) => n === bigDirSize });
}
