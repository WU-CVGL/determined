import { serverAddress } from 'routes/utils';
import { getTaskServiceCredential, getTaskServices } from 'services/api';
import { TaskServiceProtocol, V1TaskServiceState } from 'services/api-ts-sdk';
import { isDetError } from 'utils/error';

/**
 * The text of an error for a message: a DetError's public message when it is a string (the API
 * error handler copies a response's error field as it is, so it can be an object), else its message.
 */
export const errorText = (e: unknown): string => {
  if (isDetError(e)) {
    return typeof e.publicMessage === 'string' && e.publicMessage ? e.publicMessage : e.message;
  }
  return e instanceof Error ? e.message : String(e);
};

/**
 * Logs the browser in to the code-server under url with its login form, which takes the fields
 * that code-server's own form sends: base and href scope its session cookie to url, so that the
 * editors of several tasks on one master each keep their own. A login answers with a redirect, a
 * failed one with the form. code-server limits failed logins, so a failure is not retried.
 */
export const loginCodeServer = async (url: string, password: string): Promise<boolean> => {
  const login = serverAddress(`${url}login`);
  const response = await fetch(login, {
    body: new URLSearchParams({ base: url, href: login, password }).toString(),
    credentials: 'include',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    method: 'POST',
    redirect: 'manual',
  });
  return response.type === 'opaqueredirect' || (response.status >= 300 && response.status < 400);
};

/**
 * The URL to open a service of a task at, once the browser is logged in: the service must be an
 * HTTP service that is READY, the credential is read as the browser's user, and the browser logs
 * in to the service when it has a login form. Throws with the reason otherwise; a failed
 * code-server login is not retried.
 */
export const taskServiceUrl = async (taskId: string, serviceName: string): Promise<string> => {
  const { services } = await getTaskServices({ taskId });
  const service = services.find((s) => s.name === serviceName);
  if (!service) throw new Error(`No service ${serviceName}`);
  if (service.protocol === TaskServiceProtocol.TCP) {
    throw new Error(`${serviceName}: TCP`);
  }
  if (service.state !== V1TaskServiceState.READY) {
    throw new Error(`${serviceName}: ${service.state.replace('STATE_', '')}`);
  }
  const credential = await getTaskServiceCredential({ service: serviceName, taskId });
  const url = credential.url || service.url;
  if (serviceName === 'vscode') {
    const loggedIn = !!credential.password && (await loginCodeServer(url, credential.password));
    if (!loggedIn) throw new Error(`${serviceName}: login failed`);
  }
  return serverAddress(url);
};
