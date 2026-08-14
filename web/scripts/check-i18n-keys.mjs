import fs from 'node:fs'
const zh = JSON.parse(fs.readFileSync('src/locales/zh.json', 'utf8'))
const flat = (o, p = '', r = {}) => {
  for (const k of Object.keys(o)) {
    const v = o[k]
    if (v && typeof v === 'object') flat(v, p + k + '.', r)
    else r[p + k] = 1
  }
  return r
}
const keys = new Set(Object.keys(flat(zh)))
const files = ['src/pages/dashboard.tsx', 'src/pages/logs.tsx', 'src/pages/user/logs.tsx', 'src/pages/pricing.tsx', 'src/pages/templates.tsx', 'src/pages/accounts.tsx', 'src/pages/groups.tsx', 'src/pages/stats.tsx', 'src/pages/user/stats.tsx']
const re = /t\(['"`]([^'"`]+)['"`]/g
const missing = new Set()
for (const f of files) {
  const s = fs.readFileSync(f, 'utf8')
  let m
  while ((m = re.exec(s))) {
    const k = m[1]
    if (!keys.has(k)) missing.add(f + ': ' + k)
  }
}
console.log([...missing].join('\n') || 'ALL KEYS OK')
