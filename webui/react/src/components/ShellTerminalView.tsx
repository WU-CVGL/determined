import '@xterm/xterm/css/xterm.css';

import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { ITheme, Terminal } from '@xterm/xterm';
import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';

import { openTerminalLink, ShellTerminalSize } from 'services/shellTerminal';

import css from './ShellTerminalView.module.scss';

export interface ShellTerminalViewHandle {
  focus: () => void;
  reset: () => void;
  size: () => ShellTerminalSize;
  write: (data: Uint8Array | string) => void;
}

interface Props {
  darkMode: boolean;
  onInput: (data: Uint8Array) => void;
  onResize: (size: ShellTerminalSize) => void;
  onTitleChange: (title: string) => void;
}

const darkTheme: ITheme = {
  background: '#1e1e1e',
  cursor: '#d4d4d4',
  foreground: '#d4d4d4',
  selectionBackground: '#264f78',
};

const lightTheme: ITheme = {
  background: '#ffffff',
  cursor: '#1e1e1e',
  foreground: '#1e1e1e',
  selectionBackground: '#add6ff',
};

const encoder = new TextEncoder();

/**
 * An xterm.js terminal. Its output comes from a container that the shell's owner controls, so it
 * is untrusted: links open only for http(s) URLs after a confirmation, there is no clipboard
 * access (no OSC 52 addon), and window titles are passed on as plain text.
 */
const ShellTerminalView = forwardRef<ShellTerminalViewHandle, Props>(
  ({ darkMode, onInput, onResize, onTitleChange }: Props, ref) => {
    const containerRef = useRef<HTMLDivElement>(null);
    const terminalRef = useRef<Terminal>();
    const fitRef = useRef<FitAddon>();
    const callbacks = useRef({ onInput, onResize, onTitleChange });
    callbacks.current = { onInput, onResize, onTitleChange };
    const initialDarkMode = useRef(darkMode);

    useEffect(() => {
      const container = containerRef.current;
      if (!container) return;
      const terminal = new Terminal({
        cursorBlink: true,
        fontFamily: 'Menlo, Monaco, "DejaVu Sans Mono", Consolas, "Liberation Mono", monospace',
        fontSize: 14,
        // OSC 8 hyperlinks.
        linkHandler: {
          activate: (_event, text) => openTerminalLink(text),
          allowNonHttpProtocols: false,
        },
        scrollback: 10000,
        theme: initialDarkMode.current ? darkTheme : lightTheme,
      });
      const fit = new FitAddon();
      terminal.loadAddon(fit);
      // Plain-text URLs.
      terminal.loadAddon(new WebLinksAddon((_event, uri) => openTerminalLink(uri)));
      terminal.open(container);
      terminalRef.current = terminal;
      fitRef.current = fit;

      const refit = () => {
        try {
          fit.fit();
        } catch {
          // The container is hidden or not laid out yet.
        }
      };
      refit();

      const disposables = [
        terminal.onData((data) => callbacks.current.onInput(encoder.encode(data))),
        // Some mouse reports are raw bytes, one per character.
        terminal.onBinary((data) =>
          callbacks.current.onInput(Uint8Array.from(data, (c) => c.charCodeAt(0) & 0xff)),
        ),
        terminal.onResize(({ cols, rows }) => callbacks.current.onResize({ cols, rows })),
        terminal.onTitleChange((title) => callbacks.current.onTitleChange(title)),
      ];
      const observer =
        typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(() => refit());
      observer?.observe(container);
      window.addEventListener('resize', refit);

      return () => {
        window.removeEventListener('resize', refit);
        observer?.disconnect();
        disposables.forEach((d) => d.dispose());
        terminal.dispose();
        terminalRef.current = undefined;
        fitRef.current = undefined;
      };
    }, []);

    useEffect(() => {
      if (terminalRef.current) {
        terminalRef.current.options.theme = darkMode ? darkTheme : lightTheme;
      }
    }, [darkMode]);

    useImperativeHandle(
      ref,
      () => ({
        focus: () => terminalRef.current?.focus(),
        reset: () => terminalRef.current?.reset(),
        size: () => ({
          cols: terminalRef.current?.cols ?? 80,
          rows: terminalRef.current?.rows ?? 24,
        }),
        write: (data) => terminalRef.current?.write(data),
      }),
      [],
    );

    return (
      <div
        className={`${css.base} ${darkMode ? css.dark : css.light}`}
        data-testid="shell-terminal-view"
        ref={containerRef}
      />
    );
  },
);

ShellTerminalView.displayName = 'ShellTerminalView';

export default ShellTerminalView;
