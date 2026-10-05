import { Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import { observable, WritableObservable } from 'micro-observables';

import { Auth } from 'types';

/*
 * The browser's session is the master's HttpOnly session cookie: the master sets it when the user
 * signs in and removes it when they sign out, and the browser sends it with every request to the
 * master. The web UI never sees its token; this store only keeps whether the user is signed in.
 */

interface AuthState {
  auth: Loadable<Auth>;
  isChecked: boolean;
}

const defaultState: AuthState = {
  auth: NotLoaded,
  isChecked: false,
};

class AuthStore {
  #state: WritableObservable<AuthState> = observable(defaultState);

  public readonly auth = this.#state.select((state) => state.auth);
  public readonly isChecked = this.#state.select((state) => state.isChecked);
  public readonly isAuthenticated = this.auth.select((loadableAuth) => {
    return Loadable.match(loadableAuth, {
      _: () => false,
      Loaded: (a) => a.isAuthenticated,
    });
  });

  public setAuth(newAuth: Auth) {
    this.#state.update((s) => ({ ...s, auth: Loaded(newAuth) }));
  }

  public setAuthChecked() {
    this.#state.update((s) => ({ ...s, isChecked: true }));
  }

  public reset() {
    this.#state.set(defaultState);
  }
}

export default new AuthStore();
