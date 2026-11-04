// Локальная адаптация закреплённого driftty для нативного PTY (#163).
// Каждая замена проверяет число совпадений; дрейф upstream останавливает сборку.
const fs = require('node:fs');
const path = require('node:path');
const root = path.resolve(process.argv[2]);
function edit(file, replacements) {
  const filename = path.join(root, 'src/components', file);
  let source = fs.readFileSync(filename, 'utf8');
  for (const [before, after, expected = 1] of replacements) {
    const matches = typeof before === 'string'
      ? source.split(before).length - 1
      : [...source.matchAll(new RegExp(before.source, 'g'))].length;
    if (matches !== expected) throw Error(`${file}: expected ${expected} matches for ${before}, got ${matches}`);
    source = expected === 1 ? source.replace(before, after) : source.replaceAll(before, after);
  }
  fs.writeFileSync(filename, source);
}
edit('terminal-actions.ts', [
  [/  \| 'delete'\n[\s\S]*?  \| 'tmux-prefix';/, "  | 'delete';"],
  [/  'tmux-scroll': sequences.tmuxScroll,[\s\S]*?  'tmux-prefix': sequences.ctrlB,\n/, ''],
]);
edit('keyboard-overlay/keys.ts', [
  [/  tmuxScroll: .*\n  tmuxScrollExit: .*\n/, ''],
]);
edit('terminal-quickbar/mode.ts', [
  [" | 'tmux'", ''],
  [/export const quickbarModes: QuickbarMode\[\] = \[\n  'agent',\n  'nav',\n  'tmux',/, "export const quickbarModes: QuickbarMode[] = [\n  'agent',\n  'nav',"],
  [/export const keyboardQuickbarModes: QuickbarMode\[\] = \[\n  'agent',\n  'nav',\n  'tmux',/, "export const keyboardQuickbarModes: QuickbarMode[] = [\n  'agent',\n  'nav',"],
]);
edit('terminal-quickbar/controls.ts', [
  [/  tmux: \[[\s\S]*?\n  \],\n/, ''],
  [/export const scrollControls: QuickbarControl\[\] = \[[\s\S]*?\n\];\n\n/, ''],
]);
edit('terminal-menu/index.tsx', [
  [/            <MenuKeys title="tmux">[\s\S]*?<\/MenuKeys>\n/, ''],
]);
edit('terminal-quickbar/index.tsx', [
  ["  scrollControls as tmuxScrollControls,\n", ''],
  ["  scrollControls: boolean;\n", ''],
  ["    scrollControls,\n", ''],
  ["aria-label={scrollControls ? 'Scroll controls' : 'Terminal quick actions'}", 'aria-label="Terminal quick actions"'],
  [/aria-pressed=\{\n\s*scrollControls \? mode === 'tmux' : this.state.mode === mode\n\s*\}/, 'aria-pressed={this.state.mode === mode}'],
  [/                disabled=\{scrollControls\}\n/, ''],
  [/                  : mode === 'tmux'\n                    \? 'tmux'\n/, ''],
  [/aria-label=\{\n\s*scrollControls\n\s*\? 'Scroll controls'\n\s*: `\$\{this.modeLabel\(this.state.mode\)\} controls`\n\s*\}/, 'aria-label={`${this.modeLabel(this.state.mode)} controls`}'],
  ['{!scrollControls &&\n            this.state.mode', '{this.state.mode'],
  ["{!scrollControls && this.state.mode === 'copy'", "{this.state.mode === 'copy'"],
  [/                    scrollControls\n                      \? tmuxScrollControls\n                      : quickbarControls\[this.state.mode\],/, '                    quickbarControls[this.state.mode],'],
]);
edit('keyboard-overlay/index.tsx', [
  ['  scrollControls,\n', ''],
  ['  scrollControls: boolean;\n', ''],
  ['      scrollControls: false,\n', ''],
  ['this.setState({mode, scrollControls: false});', 'this.setState({mode});'],
  [/    if \('action' in control\) \{[\s\S]*?\n    \}\n/, ''],
  [/    const controls = this.state.scrollControls\n      \? scrollControls\n      : quickbarControls\[this.state.mode\];/, '    const controls = quickbarControls[this.state.mode];'],
  ["{this.state.scrollControls ? 'tmux scroll' : this.state.mode}", '{this.state.mode}'],
  ['!this.state.scrollControls && this.state.mode === mode', 'this.state.mode === mode'],
  [/                : mode === 'tmux'\n                  \? 'tmux'\n/, ''],
]);
edit('terminal/index.tsx', [
  ['  scrollControls: boolean;\n', ''],
  ['      scrollControls: false,\n', ''],
  ['      scrollControls,\n', ''],
  ['              scrollControls={scrollControls}\n', ''],
  [/    const leaveScrollControls =[\s\S]*?\n    \}\);/, '    this.clearCtrl();'],
  ['    this.setState({scrollControls: false});\n', ''],
]);
// В auto-режиме браузер не должен забирать touch-жест у xterm.
// Chromium создаёт WheelEvent с нулевыми legacy wheelDelta*, которые xterm 6
// предпочитает стандартным delta*. Убираем их только у нашего synthetic event.
edit('terminal/xterm/index.ts', [
  [/  private initTouchScroll\(\) \{\n    if \(!this.mobileClient \|\| !this.terminal.element\) return;\n    const element = this.terminal.element;/, "  private initTouchScroll() {\n    if (!this.mobileClient || !this.terminal.element) return;\n    const element = this.terminal.element;\n    element.style.touchAction = 'none';"],
  [/      const wheelTarget = this.terminal.element\?\.querySelector\(\n        '.xterm-viewport',\n      \) as HTMLElement \| null;/, "      const wheelTarget = this.terminal.element?.querySelector(\n        '.xterm-screen',\n      ) as HTMLElement | null;"],
  [/      wheelTarget.dispatchEvent\(\n        new WheelEvent\('wheel', \{([\s\S]*?)\n        \}\),\n      \);/, "      const wheelEvent = new WheelEvent('wheel', {$1\n      });\n      for (const key of ['wheelDelta', 'wheelDeltaX', 'wheelDeltaY']) {\n        Object.defineProperty(wheelEvent, key, {value: undefined});\n      }\n      wheelTarget.dispatchEvent(wheelEvent);"],
]);
// Upstream-тесты меняются вместе с контрактом, а не исключаются из typecheck.
edit('terminal/xterm/touch-selection.test.ts', [
  ['const element = new EventTarget() as EventTarget & {', 'const element = Object.assign(new EventTarget(), {style: {}}) as unknown as EventTarget & {', 3],
  ["selector === '.xterm-viewport'", "selector === '.xterm-screen'"],
  ['      readonly deltaY: number;', '      readonly deltaY: number;\n      readonly wheelDeltaY = 0;'],
  ['      deltas.push((event as WheelEvent).deltaY);', "      deltas.push((event as WheelEvent).deltaY);\n      expect((event as WheelEvent & {wheelDeltaY?: number}).wheelDeltaY).toBeUndefined();"],
]);
edit('keyboard-overlay/keys.test.ts', [
  ["defines the tmux scroll entry and page navigation sequences", "defines ordinary control and page navigation sequences"],
  ["expect(sequences.tmuxScroll).toBe('\\x02[');", "expect(sequences.ctrlB).toBe('\\x02');"],
]);
edit('terminal-actions.test.ts', [
  ['maps common terminal and tmux actions', 'maps common terminal actions'],
  ["expect(terminalActionSequence('tmux-scroll')).toBe('\\x02[');", "expect(terminalActionSequence('page-up')).toBe('\\x1b[5~');"],
  ["expect(terminalActionSequence('tmux-next')).toBe('\\x02n');", "expect(terminalActionSequence('page-down')).toBe('\\x1b[6~');"],
]);
edit('terminal-quickbar/controls.test.ts', [
  ['  scrollControls,\n', ''],
  ["    ['tmux', ['Scroll', 'Prev', 'Next', 'New']],\n", "    ['copy', []],\n"],
  [/  it\('orders the tmux scroll controls',[\s\S]*?\n  \}\);/, "  it('exposes no multiplexer controls', () => {\n    expect(Object.keys(quickbarControls)).toEqual(['agent', 'nav', 'ctrl', 'copy']);\n  });"],
]);
edit('terminal-quickbar/mode.test.ts', [
  ["memoryStorage({'ttyd-mobile:quickbar-mode': 'unknown'})", "memoryStorage({'ttyd-mobile:quickbar-mode': 'tmux'})"],
  ["saveQuickbarMode(storage, 'tmux');", "saveQuickbarMode(storage, 'nav');"],
  ["expect(loadQuickbarMode(storage)).toBe('tmux');", "expect(loadQuickbarMode(storage)).toBe('nav');"],
]);
edit('terminal-quickbar/index.test.tsx', [
  ['      scrollControls: false,\n', '', 3],
]);
edit('terminal/fixed-mobile-viewport.test.ts', [
  ['defaults a first visit on a phone-like portrait screen to 80 by 60', 'defaults a first visit on a phone-like portrait screen to fit screen'],
  [/    expect\(viewport.view.size\).toBe\('80x60'\);\n    expect\(setItem\).toHaveBeenCalledWith\(\n      'ttyd-mobile:terminal-viewport-size',\n      '80x60',\n    \);\n    expect\(setFixedSize\).toHaveBeenLastCalledWith\(\{columns: 80, rows: 60\}\);/, "    expect(viewport.view.size).toBe('auto');\n    expect(setItem).toHaveBeenCalledWith('ttyd-mobile:terminal-viewport-size', 'auto');\n    expect(setFixedSize).toHaveBeenLastCalledWith();"],
  [/  it\('uses the CSS viewport[\s\S]*?\n  \}\);/, "  it('fits a phone even when device emulation reports a desktop screen', () => {\n    const {viewport} = makeViewport({viewportSize: {width: 393, height: 852}});\n    viewport.start();\n    expect(viewport.view.size).toBe('auto');\n  });"],
  [/  it\('migrates the old 80 by 24[\s\S]*?\n  \}\);/, "  it('preserves a saved 80 by 24 choice as a custom size', () => {\n    const {viewport, setFixedSize} = makeViewport({stored: '80x24'});\n    viewport.start();\n    expect(viewport.view.size).toBe('custom:80x24');\n    expect(setFixedSize).toHaveBeenLastCalledWith({columns: 80, rows: 24});\n  });"],
]);
edit('terminal/fixed-mobile-viewport.ts', [
  [/      saveTerminalViewportSize\(storage, '80x60'\);\n      return '80x60';/, "      saveTerminalViewportSize(storage, 'custom:80x24');\n      return 'custom:80x24';"],
  [/  const shortEdge = Math.min\(viewportSize.width, viewportSize.height\);\n  const defaultSize = viewportSize.height > viewportSize.width && shortEdge <= 600\n    \? '80x60'\n    : 'auto';/, "  const defaultSize = 'auto';"],
]);
