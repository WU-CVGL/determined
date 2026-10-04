import { StorageManager } from 'utils/storage';

/*
 * The key under which earlier versions of the web UI kept the session token. The token now lives
 * only in the master's HttpOnly session cookie, which scripts cannot read, so a token left behind
 * here would be the only copy that a script on the master's origin could steal.
 */
export const LEGACY_AUTH_TOKEN_KEY = 'auth-token';

class GlobalStorage {
  private keys: Record<string, string>;
  private storage: StorageManager;

  constructor(storage: StorageManager) {
    this.storage = storage;
    this.keys = {
      landingRedirect: 'landing-redirect',
      serverAddress: 'server-address',
    };
  }

  get serverAddress() {
    return this.storage.get<string>(this.keys.serverAddress) || '';
  }

  get landingRedirect() {
    return this.storage.get<string>(this.keys.landingRedirect) || '';
  }

  set serverAddress(address: string) {
    this.storage.set(this.keys.serverAddress, address);
  }

  set landingRedirect(address: string) {
    this.storage.set(this.keys.landingRedirect, address);
  }

  removeServerAddress() {
    this.storage.remove(this.keys.serverAddress);
  }

  removeLandingRedirect() {
    this.storage.remove(this.keys.landingRedirect);
  }

  /** Removes the session token that earlier versions of the web UI stored. */
  removeLegacyAuthToken() {
    try {
      this.storage.remove(LEGACY_AUTH_TOKEN_KEY);
    } catch {
      // Storage may be unavailable, for example in a private window.
    }
  }
}

export const globalStorage = new GlobalStorage(
  new StorageManager({ basePath: 'global', store: window.localStorage }),
);

export const sessionStorage = new GlobalStorage(
  new StorageManager({ basePath: 'session', store: window.sessionStorage }),
);

globalStorage.removeLegacyAuthToken();
sessionStorage.removeLegacyAuthToken();
