import React, { RefObject, useCallback, useEffect, useRef, useState } from 'react';

/** How long the pointer rests on the button before the details show, as antd's tooltips wait. */
const HOVER_DELAY_MS = 100;

interface GpuDetailsPopup {
  buttonRef: RefObject<HTMLButtonElement>;
  close: () => void;
  onClick: (e: React.MouseEvent) => void;
  onKeyDown: (e: React.KeyboardEvent) => void;
  onMouseEnter: () => void;
  onOpenChange: (open: boolean) => void;
  peek: () => void;
  /** Shown by hover or focus, until the pointer or focus leaves the button or the popup closes. */
  peeking: boolean;
  pinned: boolean;
  popupRef: RefObject<HTMLDivElement>;
  unpeek: () => void;
}

/**
 * The open state of the GPU details popup of an info button. Hover or focus on the button peeks at
 * the details; leaving the button, a blur or Escape hides them. A click pins them. Escape, `close`,
 * a click outside the popup and its button, or a second click on the button closes the popup. A pin
 * from the keyboard moves focus into the popup, and closing the popup with focus inside gives focus
 * back to the button.
 */
const useGpuDetailsPopup = (): GpuDetailsPopup => {
  const [pinned, setPinned] = useState(false);
  const [peeking, setPeeking] = useState(false);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const popupRef = useRef<HTMLDivElement>(null);
  const hoverTimer = useRef<number>();
  const pinnedFromKeyboard = useRef(false);

  const stopHoverTimer = useCallback(() => window.clearTimeout(hoverTimer.current), []);
  const peek = useCallback(() => setPeeking(true), []);
  const unpeek = useCallback(() => {
    stopHoverTimer();
    setPeeking(false);
  }, [stopHoverTimer]);
  const onMouseEnter = useCallback(() => {
    stopHoverTimer();
    hoverTimer.current = window.setTimeout(peek, HOVER_DELAY_MS);
  }, [peek, stopHoverTimer]);
  const close = useCallback(() => {
    if (popupRef.current?.contains(document.activeElement)) buttonRef.current?.focus();
    // After the focus moves: focus on the button would show the details again.
    setPinned(false);
    unpeek();
  }, [unpeek]);
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape') close();
    },
    [close],
  );
  // Enter and Space click a button with detail 0; a pointer click has detail 1 or more.
  const onClick = useCallback(
    (e: React.MouseEvent) => {
      if (pinned) {
        close();
        return;
      }
      pinnedFromKeyboard.current = e.detail === 0;
      setPinned(true);
    },
    [close, pinned],
  );
  // antd still closes a popup on a touch outside it.
  const onOpenChange = useCallback(
    (open: boolean) => {
      if (!open) close();
    },
    [close],
  );

  // No hover timer outlives the button.
  useEffect(() => stopHoverTimer, [stopHoverTimer]);

  // Hover leaves focus where it was, so details that only peek hide on Escape wherever focus is.
  useEffect(() => {
    if (!peeking || pinned) return;
    const onDocumentKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') unpeek();
    };
    document.addEventListener('keydown', onDocumentKeyDown);
    return () => document.removeEventListener('keydown', onDocumentKeyDown);
  }, [peeking, pinned, unpeek]);

  // A pinned popup closes on a press outside it and its button.
  useEffect(() => {
    if (!pinned) return;
    const onMouseDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (!buttonRef.current?.contains(target) && !popupRef.current?.contains(target)) close();
    };
    document.addEventListener('mousedown', onMouseDown);
    return () => document.removeEventListener('mousedown', onMouseDown);
  }, [close, pinned]);

  useEffect(() => {
    if (!pinned || !pinnedFromKeyboard.current) return;
    pinnedFromKeyboard.current = false;
    // The popup may still be hidden for a frame or two while it appears.
    let frame = 0;
    let tries = 0;
    const focusDialog = () => {
      const dialog = popupRef.current;
      dialog?.focus();
      if (document.activeElement !== dialog && tries++ < 10) {
        frame = requestAnimationFrame(focusDialog);
      }
    };
    focusDialog();
    return () => cancelAnimationFrame(frame);
  }, [pinned]);

  return {
    buttonRef,
    close,
    onClick,
    onKeyDown,
    onMouseEnter,
    onOpenChange,
    peek,
    peeking,
    pinned,
    popupRef,
    unpeek,
  };
};

export default useGpuDetailsPopup;
