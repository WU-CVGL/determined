import { waitFor } from '@testing-library/react';
import { act, renderHook, RenderResult } from '@testing-library/react-hooks';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { array, boolean, number, string, undefined as undefinedType, union } from 'io-ts';
import React, { useEffect } from 'react';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getUserSetting } from 'services/api';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';

import * as hook from './useSettings';
import { SettingsProvider } from './useSettingsProvider';

const CURRENT_USER = { id: 1, isActive: true, isAdmin: false, username: 'bunny' };

vi.mock('services/api', () => ({
  getUserSetting: vi.fn(() => Promise.resolve({ settings: [] })),
  updateUserSetting: () => Promise.resolve(),
}));

interface Settings {
  boolean: boolean;
  booleanArray?: boolean[];
  number?: number;
  numberArray: number[];
  string?: string;
  stringArray?: string[];
}

interface ExtraSettings {
  extra: string;
}

type HookReturn = {
  container: RenderResult<hook.UseSettingsReturn<Settings>>;
  rerender: (
    props?:
      | {
          children: JSX.Element;
        }
      | undefined,
  ) => void;
};
type ExtraHookReturn = {
  container: RenderResult<hook.UseSettingsReturn<ExtraSettings>>;
  rerender: (
    props?:
      | {
          children: JSX.Element;
        }
      | undefined,
  ) => void;
};

const config: hook.SettingsConfig<Settings> = {
  settings: {
    boolean: {
      defaultValue: true,
      storageKey: 'boolean',
      type: boolean,
    },
    booleanArray: {
      defaultValue: undefined,
      storageKey: 'booleanArray',
      type: union([array(boolean), undefinedType]),
    },
    number: {
      defaultValue: undefined,
      storageKey: 'number',
      type: union([undefinedType, number]),
    },
    numberArray: {
      defaultValue: [-5, 0, 1e10],
      storageKey: 'numberArray',
      type: array(number),
    },
    string: {
      defaultValue: 'foo bar',
      storageKey: 'string',
      type: union([undefinedType, string]),
    },
    stringArray: {
      defaultValue: undefined,
      storageKey: 'stringArray',
      type: union([undefinedType, array(string)]),
    },
  },
  storagePath: 'settings-normal',
};

const extraConfig: hook.SettingsConfig<ExtraSettings> = {
  settings: {
    extra: {
      defaultValue: 'what',
      storageKey: 'extra',
      type: string,
    },
  },
  storagePath: 'settings-extra',
};

const Container: React.FC<{ children: JSX.Element }> = ({ children }) => {
  useEffect(() => {
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
    userStore.updateCurrentUser(CURRENT_USER);
    return userSettings.startPolling();
  }, []);

  return (
    <SettingsProvider>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <BrowserRouter>{children}</BrowserRouter>
        </ThemeProvider>
      </UIProvider>
    </SettingsProvider>
  );
};

const setup = (
  newSettings?: hook.SettingsConfig<Settings>,
  newExtraSettings?: hook.SettingsConfig<ExtraSettings>,
): {
  extraResult: ExtraHookReturn;
  result: HookReturn;
} => {
  const RouterWrapper: React.FC<{ children: JSX.Element }> = ({ children }) => (
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <Container>{children}</Container>
      </ThemeProvider>
    </UIProvider>
  );
  const hookResult = renderHook(() => hook.useSettings<Settings>(newSettings ?? config), {
    wrapper: RouterWrapper,
  });
  const extraHookResult = renderHook(
    () => hook.useSettings<ExtraSettings>(newExtraSettings ?? extraConfig),
    {
      wrapper: RouterWrapper,
    },
  );

  return {
    extraResult: { container: extraHookResult.result, rerender: extraHookResult.rerender },
    result: { container: hookResult.result, rerender: hookResult.rerender },
  };
};

describe('useSettings', () => {
  const newSettings = {
    boolean: false,
    booleanArray: [false, true],
    number: 3.14e-12,
    numberArray: [0, 100, -5280],
    string: 'Hello World',
    stringArray: ['abc', 'def', 'ghi'],
  };
  const newExtraSettings = { extra: 'fancy' };

  afterEach(() => vi.clearAllMocks());

  it('should have default settings', () => {
    const { result } = setup();
    Object.values(config.settings).forEach((configProp) => {
      const settingsKey = configProp.storageKey as keyof Settings;
      expect(result.container.current.settings[settingsKey]).toStrictEqual(configProp.defaultValue);
    });

    expect(window.location.search).toBe('');
  });

  it('should have default settings after reset', async () => {
    const { result } = setup();
    act(() => result.container.current.resetSettings());

    for (const configProp of Object.values(config.settings)) {
      const settingsKey = configProp.storageKey as keyof Settings;
      await waitFor(() =>
        expect(result.container.current.settings[settingsKey]).toStrictEqual(
          configProp.defaultValue,
        ),
      );
    }
  });

  it('should update settings', async () => {
    const { result } = setup();

    act(() => result.container.current.updateSettings(newSettings));

    for (const configProp of Object.values(config.settings)) {
      const settingsKey = configProp.storageKey as keyof Settings;
      await waitFor(() =>
        expect(result.container.current.settings[settingsKey]).toStrictEqual(
          newSettings[settingsKey],
        ),
      );
    }

    await waitFor(() => {
      expect(window.location.search).toContain(
        [
          'boolean=false',
          'booleanArray=false&booleanArray=true',
          'number=3.14e-12',
          'numberArray=0&numberArray=100&numberArray=-5280',
          'string=Hello+World',
          'stringArray=abc&stringArray=def&stringArray=ghi',
        ].join('&'),
      );
    });
  });

  it('should keep track of active settings', async () => {
    const { result } = setup();
    act(() => result.container.current.updateSettings(newSettings));

    await waitFor(() =>
      expect(result.container.current.activeSettings()).toStrictEqual(Object.keys(newSettings)),
    );
  });

  it('should be able to keep track of multiple settings', async () => {
    const { result, extraResult } = await setup();
    act(() => {
      result.container.current.updateSettings(newSettings);
      extraResult.container.current.updateSettings(newExtraSettings);
    });

    for (const configProp of Object.values(config.settings)) {
      const settingsKey = configProp.storageKey as keyof Settings & keyof ExtraSettings;
      await waitFor(() =>
        expect(result.container.current.settings[settingsKey]).toStrictEqual(
          newSettings[settingsKey],
        ),
      );
      await waitFor(() =>
        expect(extraResult.container.current.settings[settingsKey]).toStrictEqual(
          newExtraSettings[settingsKey],
        ),
      );
    }
  });
});

describe('useSettings and the URL', () => {
  interface PageSettings {
    columns: string[];
    columnWidths: number[];
    state?: string[];
    type?: string[];
  }

  /* A table's layout, which the URL leaves out, and two filters, which it shows. */
  const pageConfig: hook.SettingsConfig<PageSettings> = {
    settings: {
      columns: {
        defaultValue: ['name', 'state'],
        skipUrlEncoding: true,
        storageKey: 'columns',
        type: array(string),
      },
      columnWidths: {
        defaultValue: [200, 100],
        skipUrlEncoding: true,
        storageKey: 'columnWidths',
        type: array(number),
      },
      state: {
        defaultValue: undefined,
        storageKey: 'state',
        type: union([undefinedType, array(string)]),
      },
      type: {
        defaultValue: undefined,
        storageKey: 'type',
        type: union([undefinedType, array(string)]),
      },
    },
    storagePath: 'settings-url',
  };

  const layout = { columns: ['state', 'name', 'id'], columnWidths: [100, 200, 50] };

  /** The hook on a page without a query, once it has read these stored settings. */
  const setupWithStored = async (stored: Partial<PageSettings>) => {
    userSettings.reset();
    vi.mocked(getUserSetting).mockResolvedValueOnce({
      settings: Object.entries(stored).map(([key, value]) => ({
        key,
        storagePath: pageConfig.storagePath,
        value: JSON.stringify(value),
      })),
    });
    const Wrapper: React.FC<{ children: JSX.Element }> = ({ children }) => (
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <Container>{children}</Container>
        </ThemeProvider>
      </UIProvider>
    );
    const { result } = renderHook(() => hook.useSettings<PageSettings>(pageConfig), {
      wrapper: Wrapper,
    });
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    for (const [key, value] of Object.entries(stored)) {
      await waitFor(() =>
        expect(result.current.settings[key as keyof PageSettings]).toStrictEqual(value),
      );
    }
    return result;
  };

  const query = () => new URLSearchParams(window.location.search);

  beforeEach(() => window.history.replaceState(null, '', '/'));
  afterEach(() => window.history.replaceState(null, '', '/'));

  it('leaves the URL alone on an update of only columns and widths', async () => {
    // The stored filter is not in the URL, which only an update of a URL setting writes.
    const result = await setupWithStored({ type: ['shell'] });

    act(() => result.current.updateSettings(layout));

    await waitFor(() => expect(result.current.settings.columnWidths).toStrictEqual([100, 200, 50]));
    expect(result.current.settings.columns).toStrictEqual(['state', 'name', 'id']);
    expect(window.location.search).toBe('');
  });

  it('writes an update of a filter to the URL', async () => {
    const result = await setupWithStored({ type: ['shell'] });

    act(() => result.current.updateSettings({ type: ['experiment'] }));

    await waitFor(() => expect(query().getAll('type')).toStrictEqual(['experiment']));
    await waitFor(() => expect(result.current.settings.type).toStrictEqual(['experiment']));
  });

  it('writes the filters of an update of columns and filters together, not the columns', async () => {
    const result = await setupWithStored({ type: ['shell'] });

    act(() => result.current.updateSettings({ ...layout, state: ['active'] }));

    await waitFor(() => expect(query().getAll('state')).toStrictEqual(['active']));
    expect(query().getAll('type')).toStrictEqual(['shell']);
    expect(query().has('columns')).toBe(false);
    expect(query().has('columnWidths')).toBe(false);
    await waitFor(() => expect(result.current.settings.columns).toStrictEqual(layout.columns));
  });

  it('keeps the kinds of an update in the URL when an update of columns follows at once', async () => {
    // As on a first load: the URL's kinds replace the stored ones, then stored columns are moved.
    const result = await setupWithStored({ columns: ['name'], columnWidths: [200], type: ['a'] });

    act(() => {
      result.current.updateSettings({ type: ['b'] });
      result.current.updateSettings(layout);
    });

    await waitFor(() => expect(result.current.settings.columns).toStrictEqual(layout.columns));
    expect(result.current.settings.type).toStrictEqual(['b']);
    expect(query().getAll('type')).toStrictEqual(['b']);
  });

  const sortConfig: hook.SettingsConfig<PageSettings & { sort: string }> = {
    settings: {
      ...pageConfig.settings,
      sort: { defaultValue: 'start', storageKey: 'sort', type: string, urlFallback: true },
    },
    storagePath: pageConfig.storagePath,
  };

  it('holds the fallback setting, also at its default, in a URL without any other setting', () => {
    expect(hook.settingsToQuery(sortConfig, {})).toBe('sort=start');
    expect(hook.settingsToQuery(sortConfig, layout)).toBe('sort=start');
    expect(hook.settingsToQuery(sortConfig, { sort: 'name' })).toBe('sort=name');
    expect(hook.settingsToQuery(sortConfig, { state: ['active'] })).toBe('state=active');
    // Without a fallback, the URL of the default settings is empty.
    expect(hook.settingsToQuery(pageConfig, layout)).toBe('');
  });

  it('drops the fallback that the URL holds once another setting is in the URL', () => {
    // As the page wrote it for the default settings, which store no sort.
    window.history.replaceState(null, '', '/?sort=start');

    expect(hook.settingsToQuery(sortConfig, { state: ['active'] })).toBe('state=active');
    expect(hook.settingsToQuery(sortConfig, {})).toBe('sort=start');
    expect(hook.settingsToQuery(sortConfig, { sort: 'name' })).toBe('sort=name');

    // A setting that the URL leaves out is no other setting.
    window.history.replaceState(null, '', '/?columns=name');
    expect(hook.settingsToQuery(sortConfig, {})).toBe('columns=name&sort=start');
  });
});
