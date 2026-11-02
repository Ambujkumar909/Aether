import test from 'node:test';
import assert from 'node:assert/strict';
import { renderBody, inline, linkify, wrapSelection, suggestedReplies } from './format.js';

// Formatting is applied to ESCAPED text and only emits its own tags. The
// first test is the one that matters: nothing a message contains becomes
// markup.
test('markup in a message stays text', () => {
  assert.equal(renderBody('<img src=x onerror=alert(1)>'), '&lt;img src=x onerror=alert(1)&gt;');
  assert.equal(renderBody('**<b>x</b>**'), '<strong>&lt;b&gt;x&lt;/b&gt;</strong>');
  assert.equal(renderBody('`<script>`'), '<code>&lt;script&gt;</code>');
  assert.ok(!renderBody('javascript:alert(1)').includes('<a'), 'no link for a javascript: scheme');
});

test('bold, italic, strike, code', () => {
  assert.equal(inline('a **b** c'), 'a <strong>b</strong> c');
  assert.equal(inline('a *b* c'), 'a <em>b</em> c');
  assert.equal(inline('~~gone~~'), '<s>gone</s>');
  assert.equal(inline('use `x*y*z` here *ok*'), 'use <code>x*y*z</code> here <em>ok</em>', 'code protects its contents');
  assert.equal(inline('2*3*4'), '2*3*4', 'arithmetic is not italics');
  assert.equal(inline('** **'), '** **', 'empty marks stay literal');
});

test('urls become links, punctuation stays prose', () => {
  assert.equal(
    linkify('see https://example.com/a?b=1, ok'),
    'see <a href="https://example.com/a?b=1" target="_blank" rel="noopener noreferrer">https://example.com/a?b=1</a>, ok'
  );
  assert.equal(linkify('(https://x.y/z)'), '(<a href="https://x.y/z" target="_blank" rel="noopener noreferrer">https://x.y/z</a>)');
});

test('fenced code keeps everything and numbers the lines', () => {
  const out = renderBody('before\n```go\nfmt.Println("<hi>")\n**not bold**\n```\nafter *em*');
  assert.ok(out.startsWith('before<br>'));
  assert.ok(out.includes('<pre class="code" data-lang="go"><code><span class="ln">1</span>fmt.Println(&quot;&lt;hi&gt;&quot;)'));
  assert.ok(out.includes('<span class="ln">2</span>**not bold**'), 'no formatting inside code');
  assert.ok(out.endsWith('after <em>em</em>'));
  assert.ok(renderBody('```\nopen').includes('<pre class="code">'), 'an unclosed fence is code to the end');
});

test('wrapping a selection', () => {
  assert.deepEqual(wrapSelection('hello world', 6, 11, '**'), { text: 'hello **world**', start: 8, end: 13 });
  assert.deepEqual(wrapSelection('hi', 2, 2, '`'), { text: 'hi``', start: 3, end: 3 });
});

test('suggested replies only for questions', () => {
  assert.deepEqual(suggestedReplies('Can you send the deck?'), ['Yes, on it', 'Not right now', 'Let me check']);
  assert.deepEqual(suggestedReplies('When is the review?'), ['In a few minutes', 'Later today', 'Tomorrow']);
  assert.deepEqual(suggestedReplies('Deployed.'), []);
  assert.deepEqual(suggestedReplies('a?'.padStart(300, 'x')), [], 'a long message is not a quick question');
});

import { plainText } from './format.js';
test('previews drop the marks', () => {
  assert.equal(plainText('ship **today** with `go build`'), 'ship today with go build');
  assert.equal(plainText('```js\nlet x\n```\ndone'), 'let x done');
  assert.equal(plainText('2*3*4'), '2*3*4');
});
