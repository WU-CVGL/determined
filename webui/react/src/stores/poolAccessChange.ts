import { observable, WritableObservable } from 'micro-observables';

import { PoolAccessAction, PoolAccessResult } from 'utils/resourcePoolAccess';

/** A change of pool access: running until it has its results. */
export interface PoolAccessChange {
  action: PoolAccessAction;
  results?: PoolAccessResult[];
}

const running = (change: PoolAccessChange | undefined): boolean => !!change && !change.results;

/**
 * The pool access change of this app instance. Only one runs at a time, so a change cannot send
 * its requests after a newer one. The change outlives its dialog and the Pool Access tab, which
 * show it until it is dismissed.
 */
class PoolAccessChangeStore {
  #change: WritableObservable<PoolAccessChange | undefined> = observable(undefined);
  /** The Pool Access tabs that are shown. */
  #tabs = 0;

  public readonly change = this.#change.readOnly();
  public readonly isRunning = this.#change.select(running);

  /**
   * Runs a change and answers with its results, or with undefined, sending nothing, while another
   * change runs. The check and the start happen before anything is awaited.
   */
  public async run(
    action: PoolAccessAction,
    send: () => Promise<PoolAccessResult[]>,
  ): Promise<PoolAccessResult[] | undefined> {
    if (running(this.#change.get())) return undefined;
    this.#change.set({ action });
    let results: PoolAccessResult[];
    try {
      results = await send();
    } catch (e) {
      this.#change.set(undefined);
      throw e;
    }
    this.#change.set({ action, results });
    return results;
  }

  /** Forgets a finished change; a running one stays. */
  public dismiss(): void {
    if (!running(this.#change.get())) this.#change.set(undefined);
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
