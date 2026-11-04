import { act, render, screen } from '@testing-library/react';
import { createRef } from 'react';

import ShellTerminalView, { ShellTerminalViewHandle } from './ShellTerminalView';

const xterm = vi.hoisted(() => ({
  fit: vi.fn(),
  resize: undefined as undefined | ((size: { cols: number; rows: number }) => void),
}));

vi.mock('@xterm/xterm/css/xterm.css', () => ({}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    options = {};
    rows = 24;
    dispose() {}
    loadAddon() {}
    onBinary() {
      return { dispose() {} };
    }
    onData() {
      return { dispose() {} };
    }
    onResize(handler: (size: { cols: number; rows: number }) => void) {
      xterm.resize = handler;
      return { dispose() {} };
    }
    onTitleChange() {
      return { dispose() {} };
    }
    open() {}
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = xterm.fit;
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));

const FITTED = { cols: 10, rows: 6 };

/* The observers of the containers, to report a layout change as the browser would. */
const observers = vi.hoisted(() => [] as (() => void)[]);

/* Gives the container a layout size, as the browser does once it is shown, or none when hidden. */
const layOut = (container: HTMLElement, width: number, height: number) => {
  Object.defineProperty(container, 'clientWidth', { configurable: true, value: width });
  Object.defineProperty(container, 'clientHeight', { configurable: true, value: height });
};

const setup = () => {
  const ref = createRef<ShellTerminalViewHandle>();
  const onResize = vi.fn();
  render(
    <ShellTerminalView
      darkMode={false}
      ref={ref}
      onInput={vi.fn()}
      onResize={onResize}
      onTitleChange={vi.fn()}
    />,
  );
  return { container: screen.getByTestId('shell-terminal-view'), onResize, ref };
};

describe('ShellTerminalView', () => {
  beforeEach(() => {
    // The addon resizes the terminal to what it measures, as it does with a hidden container.
    xterm.fit.mockReset().mockImplementation(() => xterm.resize?.(FITTED));
    observers.length = 0;
    vi.stubGlobal(
      'ResizeObserver',
      class {
        constructor(callback: () => void) {
          observers.push(callback);
        }
        disconnect() {}
        observe() {}
      },
    );
  });

  afterEach(() => vi.unstubAllGlobals());

  it('does not fit the terminal while its container has no size', () => {
    const { onResize, ref } = setup();
    expect(observers).toHaveLength(1);
    expect(xterm.fit).not.toHaveBeenCalled();
    act(() => observers[0]());
    window.dispatchEvent(new Event('resize'));
    act(() => ref.current?.fit());
    expect(xterm.fit).not.toHaveBeenCalled();
    expect(onResize).not.toHaveBeenCalled();
  });

  it('fits the terminal to a container with a size, on a layout change and on request', () => {
    const { container, onResize, ref } = setup();
    layOut(container, 800, 600);
    act(() => observers[0]());
    expect(xterm.fit).toHaveBeenCalledTimes(1);
    window.dispatchEvent(new Event('resize'));
    expect(xterm.fit).toHaveBeenCalledTimes(2);
    act(() => ref.current?.fit());
    expect(xterm.fit).toHaveBeenCalledTimes(3);
    expect(onResize).toHaveBeenCalledTimes(3);
    expect(onResize).toHaveBeenCalledWith(FITTED);
  });

  it('keeps the size of a hidden terminal and fits it once when shown again', () => {
    const { container, onResize, ref } = setup();
    layOut(container, 800, 600);
    act(() => ref.current?.fit());
    expect(xterm.fit).toHaveBeenCalledTimes(1);
    expect(onResize).toHaveBeenCalledTimes(1);

    // Hidden by the tabs: the browser reports the change, and nothing is fitted or resized.
    layOut(container, 0, 0);
    act(() => observers[0]());
    window.dispatchEvent(new Event('resize'));
    expect(xterm.fit).toHaveBeenCalledTimes(1);
    expect(onResize).toHaveBeenCalledTimes(1);

    layOut(container, 1024, 768);
    act(() => ref.current?.fit());
    expect(xterm.fit).toHaveBeenCalledTimes(2);
    expect(onResize).toHaveBeenCalledTimes(2);
  });
});
