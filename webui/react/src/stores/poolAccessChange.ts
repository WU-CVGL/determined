import { observable, WritableObservable } from 'micro-observables';

import { PoolAccessAction, PoolAccessResult } from 'utils/resourcePoolAccess';

/** A change of pool access: running until it has its results. */
export interface PoolAccessChange {
  action: PoolAccessAction;
  results?: PoolAccessResult[];
}

export const isChangeRunning = (change: PoolAccessChange | undefined): boolean =>
  !!change && !change.results;

/**
 * The pool access change of this app instance. Only one runs at a time, so a change cannot send
 * its requests after a newer one. The change outlives its dialog and the Pool Access tab, which
 * show it until it is dismissed. Signing out resets it.
 */
class PoolAccessChangeStore {
  #change: WritableObservable<PoolAccessChange | undefined> = observable(undefined);
  /** Aborts the running change. */
  #canceler?: AbortController;
  /** The Pool Access tabs that are shown. */
  #tabs = 0;

  public readonly change = this.#change.readOnly();
  public readonly isRunning = this.#change.select(isChangeRunning);

  /**
   * Runs a change and answers with its results. It answers undefined, sending nothing, while
   * another change runs, and undefined when the change was reset before it ended. The check and
   * the start happen before anything is awaited. send gets the signal that reset aborts.
   */
  public async run(
    action: PoolAccessAction,
    send: (signal: AbortSignal) => Promise<PoolAccessResult[]>,
  ): Promise<PoolAccessResult[] | undefined> {
    if (isChangeRunning(this.#change.get())) return undefined;
    const change: PoolAccessChange = { action };
    const canceler = new AbortController();
    this.#canceler = canceler;
    this.#change.set(change);
    // After a reset, the change no longer writes to the store.
    const isCurrent = () => this.#change.get() === change;
    try {
      const results = await send(canceler.signal);
      if (!isCurrent()) return undefined;
      this.#change.set({ action, results });
      return results;
    } catch (e) {
      if (isCurrent()) this.#change.set(undefined);
      throw e;
    } finally {
      if (this.#canceler === canceler) this.#canceler = undefined;
    }
  }

  /** Forgets a finished change; a running one stays. */
  public dismiss(): void {
    if (!isChangeRunning(this.#change.get())) this.#change.set(undefined);
  }

  /** Aborts the running change, which then sends no more requests, and forgets the change. */
  public reset(): void {
    this.#canceler?.abort();
    this.#canceler = undefined;
    this.#change.set(undefined);
  }

  /** Counts a shown Pool Access tab until the returned function is called. */
  public showTab(): () => void {
    this.#tabs += 1;
    return () => {
      this.#tabs -= 1;
    };
  }

  public get isTabShown(): boolean {
    return this.#tabs > 0;
  }
}

export default new PoolAccessChangeStore();
