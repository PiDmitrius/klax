// markdown.js — the in-browser Markdown→HTML engine, extracted from the old monolith
// (working, security-reviewed code: escape-first, code-span isolation, bounded regexes,
// our /api/file capability images). Pure functions; the only dependency is apiHref for
// mount-aware image/link URLs.

import { apiHref } from "./base.js";

export function esc(s){ return (s||"").replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
export function stripTags(s){ return (s||"").replace(/<[^>]+>/g, ""); }

// --- time (hover-revealed stamp: Y.m.d over H:i:s) ---
export function fmtDate(unix){
  if(!unix) return "";
  const d = new Date(unix), p = n => String(n).padStart(2,"0");
  return d.getFullYear()+"."+p(d.getMonth()+1)+"."+p(d.getDate());
}
export function fmtTime(unix){
  if(!unix) return "";
  const d = new Date(unix), p = n => String(n).padStart(2,"0");
  return p(d.getHours())+":"+p(d.getMinutes())+":"+p(d.getSeconds());
}
export function fmtDelta(ms){
  if(ms < 1000) return ms+"ms";
  const s = ms/1000; if(s < 60) return s.toFixed(1)+"s";
  const m = s/60;    if(m < 60) return m.toFixed(1)+"min";
  return (m/60).toFixed(1)+"h";
}

// Inline-level Markdown for a single line.
export function inline(s){
  // Pull `code` spans out as placeholders FIRST so their content is never transformed (a *,
  // [ or | inside code stays literal), yet bold/italic/links can still SPAN a code span —
  // the old split-on-code approach put the ** open/close in separate segments, so bold
  // wrapping code never matched (the screenshot bug). The delimiter is a NUL char, which can
  // never appear in real text; _underscore_ emphasis stays unsupported (snake_case fills
  // this tool's output).
  const Z = String.fromCharCode(0);
  s = (s || "").split(Z).join("");
  const codes = [];
  let h = s.replace(/`[^`]+`/g, m => { codes.push("<code>" + esc(m.slice(1, -1)) + "</code>"); return Z + (codes.length - 1) + Z; });
  h = esc(h);
  if(h.indexOf("**") !== -1) h = h.replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
  if(h.indexOf("~~") !== -1) h = h.replace(/~~([^~]+)~~/g, '<s>$1</s>');
  if(h.indexOf("*")  !== -1) h = h.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<i>$2</i>'); // *italic*, never part of **bold**
  // Images: ![alt](url) -> <img> for our /api/file capability refs or remote http(s).
  if(h.indexOf("![") !== -1)
    h = h.replace(/!\[([^\]]*)\]\((\/api\/file\?[^)\s]+|https?:\/\/[^)\s]+)\)/g,
      (m, alt, href) => '<img class="att" src="'+apiHref(href)+'" alt="'+alt+'" loading="lazy"'+imageSizeAttrs(href)+'>');
  // Bound the link regex (it backtracks O(n^2) on bracket-heavy text with no early "]").
  if(h.indexOf("](") !== -1 && h.length < 50000 && (h.match(/\[/g) || []).length < 200)
    h = h.replace(/\[([^\]]+)\]\(([^)]+)\)/g, function(m, text, href){
      if(/^https?:\/\/[^\s]+$/.test(href)) return '<a href="'+href+'" target="_blank" rel="noopener">'+text+'</a>';
      if(/^\/api\/file\?/.test(href)) return '<a href="'+apiHref(href)+'" target="_blank" rel="noopener">'+text+'</a>'; // our file capability URL
      return '<u>'+text+'</u>';
    });
  // Restore code spans (already escaped); linkifyBare then leaves <code>/<a>/<u> alone.
  h = h.replace(new RegExp(Z + "(\\d+)" + Z, "g"), (m, i) => codes[+i] || "");
  return linkifyBare(h);
}

function imageSizeAttrs(href){
  const wm = href.match(/(?:[?&]|&amp;)w=(\d{1,5})/);
  const hm = href.match(/(?:[?&]|&amp;)h=(\d{1,5})/);
  if(!wm || !hm) return "";
  const w = parseInt(wm[1], 10), h = parseInt(hm[1], 10);
  if(w <= 0 || h <= 0) return "";
  return ' width="'+w+'" height="'+h+'"';
}

// Turn bare http(s) URLs into links, leaving existing HTML tags and protected spans
// untouched. This must not linkify URLs inside attributes of HTML we just generated,
// such as <img src="https://...">.
// Safe: h is already escaped, so a URL match holds no raw <, >, or "; only https?:// matches.
export function linkifyBare(h){
  return h.replace(/(<a\b[^>]*>[\s\S]*?<\/a>|<code>[\s\S]*?<\/code>|<u>[\s\S]*?<\/u>|<[^>]+>)|(https?:\/\/[^\s<]+)/g, function(m, span, url){
    if(span) return span;
    var u = url, tail = "";
    var p = u.match(/[.,;:!?]+$/);
    if(p){ tail = p[0]; u = u.slice(0, -p[0].length); }
    var opens = 0, closes = 0;
    for(var k = 0; k < u.length; k++){ var c = u[k]; if(c === "(") opens++; else if(c === ")") closes++; }
    var t = 0; while(t < u.length && u[u.length-1-t] === ")") t++;
    var strip = Math.min(t, closes - opens);
    if(strip > 0){ tail = u.slice(u.length - strip) + tail; u = u.slice(0, u.length - strip); }
    return '<a href="' + u + '" target="_blank" rel="noopener">' + u + '</a>' + tail;
  });
}

// Split a table row into trimmed cells on top-level pipes only.
function splitRow(line){
  const s = line.trim(), cells = [];
  let cur = "", code = false;
  for(let i = 0; i < s.length; i++){
    const ch = s[i];
    if(ch === "`"){ code = !code; cur += ch; }
    else if(ch === "\\" && s[i+1] === "|"){ cur += "|"; i++; }
    else if(ch === "|" && !code){ cells.push(cur.trim()); cur = ""; }
    else cur += ch;
  }
  cells.push(cur.trim());
  if(cells.length && cells[0] === "") cells.shift();
  if(cells.length && cells[cells.length-1] === "") cells.pop();
  return cells;
}

function renderTable(header, aligns, rows){
  const cell = (tag, c, i) => '<'+tag+(aligns[i] ? ' style="text-align:'+aligns[i]+'"' : '')+'>'+inline(c||"")+'</'+tag+'>';
  const head = '<tr>'+header.map((c,i)=>cell("th",c,i)).join("")+'</tr>';
  const body = rows.map(r => '<tr>'+header.map((_,i)=>cell("td",r[i],i)).join("")+'</tr>').join("");
  return '<table><thead>'+head+'</thead><tbody>'+body+'</tbody></table>';
}

function renderList(lines){
  let out = "";
  const stack = [];
  const close = () => { const t = stack.pop(); out += "</li>" + (t.type === "ol" ? "</ol>" : "</ul>"); };
  const open = (indent, type, start) => { out += (type === "ol" ? (start > 1 ? '<ol start="'+start+'">' : "<ol>") : "<ul>"); stack.push({indent, type}); };
  lines.forEach(raw => {
    const m = raw.match(/^(\s*)(?:([-*+])|(\d+)[.)])\s+(.*)$/);
    if(!m) return;
    const indent = m[1].replace(/\t/g, "  ").length;
    const type = m[3] ? "ol" : "ul";
    const start = m[3] ? parseInt(m[3], 10) : 0;
    while(stack.length && indent < stack[stack.length-1].indent) close();
    const top = stack[stack.length-1];
    if(!top || indent > top.indent) open(indent, type, start);
    else if(top.type !== type){ close(); open(indent, type, start); }
    else out += "</li>";
    out += "<li>" + inline(m[4]);
  });
  while(stack.length) close();
  return out;
}

// Block-level Markdown -> HTML: headings, paragraphs, ordered & nested lists, blockquotes,
// pipe tables (with alignment), fenced code, thematic breaks. Code fences are isolated
// first so their contents are never transformed.
function md(src, depth){
  depth = depth || 0;
  let html = "";
  (src||"").split(/(```[\s\S]*?```)/g).forEach(seg => {
    const fence = seg.match(/^```(\w*)\n?([\s\S]*?)```$/);
    if(fence){ html += '<pre><button class="copy" title="Копировать">⧉</button><code>'+esc(fence[2].replace(/\n+$/,""))+'</code></pre>'; return; }
    const lines = seg.split("\n");
    let para = [];
    const flush = () => { if(para.length){ html += '<p>'+para.map(inline).join("<br>")+'</p>'; para = []; } };
    for(let i = 0; i < lines.length; i++){
      const line = lines[i];
      if(line.trim() === ""){ flush(); continue; }
      const hm = line.match(/^(#{1,6})\s+(.*)$/);
      if(hm){ flush(); const l = hm[1].length; html += '<h'+l+'>'+inline(hm[2])+'</h'+l+'>'; continue; }
      if(/^(-{3,}|\*{3,}|_{3,})$/.test(line.trim())){ flush(); html += '<hr>'; continue; }
      if(/^\s*>/.test(line)){
        flush();
        const q = [];
        while(i < lines.length && /^\s*>/.test(lines[i])){ q.push(lines[i].replace(/^\s*>\s?/, "")); i++; }
        i--;
        const inner = q.join("\n");
        html += '<blockquote>'+(depth < 8 ? md(inner, depth+1) : '<p>'+esc(inner)+'</p>')+'</blockquote>';
        continue;
      }
      if(line.indexOf("|") !== -1 && i+1 < lines.length &&
         /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)+\|?\s*$/.test(lines[i+1])){
        flush();
        const header = splitRow(line);
        const aligns = splitRow(lines[i+1]).map(c => {
          const l = c.startsWith(":"), r = c.endsWith(":");
          return l && r ? "center" : r ? "right" : l ? "left" : "";
        });
        i += 2;
        const rows = [];
        while(i < lines.length && lines[i].trim() !== "" && lines[i].indexOf("|") !== -1){ rows.push(splitRow(lines[i])); i++; }
        i--;
        html += renderTable(header, aligns, rows);
        continue;
      }
      if(/^\s*([-*+]|\d+[.)])\s+/.test(line)){
        flush();
        const items = [];
        while(i < lines.length){
          if(/^\s*([-*+]|\d+[.)])\s+/.test(lines[i])){ items.push(lines[i]); i++; }
          else if(lines[i].trim() === "" && i+1 < lines.length && /^\s*([-*+]|\d+[.)])\s+/.test(lines[i+1])){ i++; }
          else break;
        }
        i--;
        html += renderList(items);
        continue;
      }
      para.push(line);
    }
    flush();
  });
  return html;
}

// mdSafe wraps md() so a pathological block degrades to escaped text with line breaks
// instead of throwing. ALL render paths use this, never md() directly.
export function mdSafe(src){
  try { return md(src); }
  catch(e){ console.error("klax md()", e); return esc(src).replace(/\n/g, "<br>"); }
}
