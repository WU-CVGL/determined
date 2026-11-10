import dayjs from 'dayjs';
import utc from 'dayjs/plugin/utc';
import { Failed, Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import { useCallback, useEffect, useRef, useState } from 'react';

import { getUserUsage } from 'services/api';
import { V1ResourceAllocationAggregationPeriod } from 'services/api-ts-sdk';
import { UserUsage } from 'types';
import handleError from 'utils/error';
import { isAborted } from 'utils/service';

import { DAY_FORMAT, todayUtc, usageRange } from './usage';

dayjs.extend(utc);

/** The time between two usage loads while the page is visible. */
export const USAGE_REFRESH_INTERVAL = 5 * 60_000;
/** The age of the last load above which the page loads the usage again when it is shown. */
export const USAGE_STALE_AFTER = 60_000;
/** The delay after a UTC midnight before the new day is taken. */
const DAY_CHANGE_DELAY = 1000;

interface LastLoad {
  /** When the load ended. */
  at: number;
  /** The UTC day the load was for. */
  day: string;
}

export interface UserUsageState {
  /** The start of the UTC day that the range, the grid and the views end with. */
  now: number;
  usage: Loadable<UserUsage>;
}

/**
 * The daily usage of a user, for the calendar, the days and the months: loaded when shown,
 * again at the UTC day change, every five minutes while the page is visible, and when the page is
 * shown again after a minute. Nothing loads while the page is hidden; a failed refresh keeps the
 * usage shown.
 */
/** Whether the page is hidden, from the document itself: also for a page opened hidden. */
const usePageHidden = (): boolean => {
  const [hidden, setHidden] = useState(() => document.hidden);
  useEffect(() => {
    const update = () => setHidden(document.hidden);
    update();
    document.addEventListener('visibilitychange', update);
    return () => document.removeEventListener('visibilitychange', update);
  }, []);
  return hidden;
};

const useUserUsage = (userId: number): UserUsageState => {
  const hidden = usePageHidden();
  const [usage, setUsage] = useState<Loadable<UserUsage>>(NotLoaded);
  const [day, setDay] = useState(() => todayUtc().format(DAY_FORMAT));
  const [lastLoad, setLastLoad] = useState<LastLoad>();
  const hasLoaded = useRef(false);
  const hasFailed = useRef(false);
  const pendingDay = useRef<string>();
  const sequence = useRef(0);
  const canceler = useRef(new AbortController());

  // Answers that arrive after the unmount are dropped.
  useEffect(() => {
    const current = new AbortController();
    canceler.current = current;
    return () => current.abort();
  }, []);

  const load = useCallback(
    async (day: string) => {
      const request = ++sequence.current;
      pendingDay.current = day;
      const { endDate, startDate } = usageRange(dayjs.utc(day).valueOf());
      const signal = canceler.current.signal;
      const current = () => !signal.aborted && request === sequence.current;
      try {
        const response = await getUserUsage(
          {
            endDate,
            period: V1ResourceAllocationAggregationPeriod.DAILY,
            startDate,
            userId,
          },
          { signal },
        );
        if (!current()) return;
        hasLoaded.current = true;
        setUsage(Loaded(response));
      } catch (e) {
        if (!current() || isAborted(e)) return;
        // A toast for the first failure only, not for every refresh after it.
        handleError(e, {
          publicSubject: 'Unable to fetch usage.',
          silent: hasLoaded.current || hasFailed.current,
        });
        hasFailed.current = true;
        if (!hasLoaded.current) setUsage(Failed(e as Error));
      }
      pendingDay.current = undefined;
      setLastLoad({ at: Date.now(), day });
    },
    [userId],
  );

  useEffect(() => {
    if (hidden) return;
    const now = Date.now();
    const today = todayUtc(now);
    const todayKey = today.format(DAY_FORMAT);
    if (todayKey !== day) {
      setDay(todayKey);
      return;
    }
    const timers: ReturnType<typeof setTimeout>[] = [];
    const nextDay = today.add(1, 'day').valueOf() - now + DAY_CHANGE_DELAY;
    timers.push(setTimeout(() => setDay(todayUtc().format(DAY_FORMAT)), nextDay));
    if (pendingDay.current !== day) {
      if (!lastLoad || lastLoad.day !== day || now - lastLoad.at >= USAGE_STALE_AFTER) {
        load(day);
      } else {
        const next = lastLoad.at + USAGE_REFRESH_INTERVAL - now;
        timers.push(setTimeout(() => load(day), next));
      }
    }
    return () => timers.forEach((timer) => clearTimeout(timer));
  }, [day, hidden, lastLoad, load]);

  return { now: dayjs.utc(day).valueOf(), usage };
};

export default useUserUsage;
