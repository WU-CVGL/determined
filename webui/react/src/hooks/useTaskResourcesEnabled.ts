import { Loadable } from 'hew/utils/loadable';
import { useEffect, useState } from 'react';

import { serverAddress } from 'routes/utils';
import userStore from 'stores/users';

// Row menus mount together. Share their in-flight capability request without
// retaining an authorization-dependent result across later mounts or sign-ins.
// The session cookie authenticates the request, so the signed-in user tells sessions apart.
const pendingCapabilities = new Map<string, Promise<boolean>>();
const loadCapability = (): Promise<boolean> => {
  const url = serverAddress('/ui/task-resources');
  const userId = Loadable.getOrElse(undefined, userStore.currentUser.get())?.id;
  const key = JSON.stringify([url, userId]);
  const pending = pendingCapabilities.get(key);
  if (pending) return pending;
  const controller = new AbortController();
  const timeout = window.setTimeout(() => controller.abort(), 10000);
  const request = fetch(url, {
    credentials: 'include',
    signal: controller.signal,
  })
    .then(async (response) => (response.ok ? (await response.json()).enabled === true : false))
    .catch(() => false)
    .finally(() => {
      window.clearTimeout(timeout);
      pendingCapabilities.delete(key);
    });
  pendingCapabilities.set(key, request);
  return request;
};

const useTaskResourcesEnabled = (): boolean | undefined => {
  const [enabled, setEnabled] = useState<boolean>();
  useEffect(() => {
    let active = true;
    loadCapability().then((value) => {
      if (active) setEnabled(value);
    });
    return () => {
      active = false;
    };
  }, []);
  return enabled;
};

export default useTaskResourcesEnabled;
