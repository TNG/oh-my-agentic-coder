"""Build an explainer page from meta.json + content.html and the skill's template.

Usage:
    python3 scripts/build.py <work-dir>               # page for a Claude Artifact (no html/head/body)
    python3 scripts/build.py <work-dir> --standalone  # complete HTML file for a browser

Reads <work-dir>/meta.json and <work-dir>/content.html, writes <work-dir>/explainer.html,
and prints a JSON summary. "problems" must be an empty list before you publish.
"""
import html, json, os, re, sys

SKILL = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MERMAID = '''<script src="https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.min.js"></script>
<script>mermaid.initialize({startOnLoad: true, theme: matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "default"});</script>'''
SECTIONS = ['what', 'context', 'scenarios', 'approaches', 'review']

work = sys.argv[1]
standalone = '--standalone' in sys.argv[2:]
meta = json.load(open(os.path.join(work, 'meta.json')))
content = open(os.path.join(work, 'content.html')).read()
template = open(os.path.join(SKILL, 'template', 'page.html')).read()
esc = html.escape

problems = []
for key in ('repo', 'number', 'title', 'lede', 'head'):
    if not meta.get(key):
        problems.append(f'meta.json: missing "{key}"')
secs = re.findall(r'<section id="([a-z0-9-]+)">\s*<h2>(.*?)</h2>', content, re.S)
if [i for i, _ in secs] != SECTIONS:
    problems.append(f'sections must be {SECTIONS} in this order, found {[i for i, _ in secs]}')
for tag in ('section', 'div', 'table', 'pre', 'figure', 'ul', 'ol', 'details'):
    opened = len(re.findall(rf'<{tag}[\s>]', content))
    closed = content.count(f'</{tag}>')
    if opened != closed:
        problems.append(f'<{tag}> opened {opened} times, closed {closed} times')
if re.search(r'<(script|style|link|iframe|img|html|head|body)\b', content):
    problems.append('content.html must not contain script, style, link, iframe, img, html, head or body')

repo = meta.get('repo', '')
number = str(meta.get('number', ''))
values = {
    'title': esc(meta.get('title', '')),
    'lede': meta.get('lede', ''),
    'facts': ''.join(f'<span>{esc(f)}</span>' for f in meta.get('facts', [])),
    'pr_url': f'https://github.com/{repo}/pull/{number}',
    'repo_name': esc(repo.split('/')[-1]),
    'number': esc(number),
    'head_short': esc(meta.get('head', '')[:7]),
    'toc': ''.join(f'<li><a href="#{i}">{t}</a></li>' for i, t in secs),
    'content': content,
}
page = re.sub(r'\{\{(\w+)\}\}', lambda m: values[m.group(1)], template)
if standalone:
    page = ('<!doctype html>\n<html lang="en"><head><meta charset="utf-8">\n'
            '<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n'
            + page.replace('</style>', '</style>\n</head><body>', 1)
            + MERMAID + '\n</body></html>\n')
out = os.path.join(work, 'explainer.html')
open(out, 'w').write(page)
print(json.dumps({'output': out, 'mode': 'standalone' if standalone else 'artifact',
                  'sections': [i for i, _ in secs], 'bytes': len(page), 'problems': problems}))
