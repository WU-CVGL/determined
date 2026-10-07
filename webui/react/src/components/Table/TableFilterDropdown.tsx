import { Checkbox } from 'antd';
import { FilterDropdownProps } from 'antd/es/table/interface';
import Button from 'hew/Button';
import Icon from 'hew/Icon';
import Input, { InputRef } from 'hew/Input';
import { useTheme } from 'hew/Theme';
import React, { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { FixedSizeList, ListChildComponentProps } from 'react-window';

import usePrevious from 'hooks/usePrevious';

import css from './TableFilterDropdown.module.scss';

interface Props extends FilterDropdownProps {
  /**
   * A tick list: each option has a checkbox, the footer has All, None and OK, the list is one tab
   * stop (Up and Down move, Space ticks, Enter applies), Tab goes round the dropdown, and Escape
   * closes it without applying. Ticking nothing or every option applies no filter (no keys).
   */
  checklist?: boolean;
  /** The tick list's name, for assistive technology. */
  label?: string;
  multiple?: boolean;
  onFilter?: (keys: string[]) => void;
  onReset?: () => void;
  searchable?: boolean;
  values?: string[];
  width?: number;
}

export const ARIA_LABEL_CONTAINER = 'table-filter-dropdown-container';
export const ARIA_LABEL_INPUT = 'table-filter-dropdown-input';
export const ARIA_LABEL_RESET = 'table-filter-reset';
export const ARIA_LABEL_APPLY = 'table-filter-apply';

const ITEM_HEIGHT = 28;
/* A tick list without a search box shows this many options before it scrolls. */
const CHECKLIST_ROWS = 12;

const TableFilterDropdown: React.FC<Props> = ({
  checklist,
  clearFilters,
  close,
  confirm,
  filters,
  label,
  multiple,
  onFilter,
  onReset,
  searchable,
  values = [],
  visible,
  width = 160,
}: Props) => {
  const inputRef = useRef<InputRef>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const virtualListRef = useRef<FixedSizeList>(null);
  const footerRef = useRef<HTMLDivElement>(null);
  // The element that had the focus when the dropdown opened, which gets it back when it closes.
  const openerRef = useRef<HTMLElement | null>(null);
  const listId = useId();
  const [search, setSearch] = useState('');
  const [selectedMap, setSelectedMap] = useState<Record<string, boolean>>({});
  const [activeIndex, setActiveIndex] = useState(0);
  const prevVisible = usePrevious(visible, undefined);
  const {
    themeSettings: { className: themeClass },
  } = useTheme();
  const filteredOptions = useMemo(() => {
    const searchString = search.toLocaleLowerCase();
    return (filters || []).filter((filter) => {
      // A tick list searches the text it shows.
      if (checklist && typeof filter.text === 'string') {
        return filter.text.toLocaleLowerCase().includes(searchString);
      }
      return (
        filter.value?.toString().toLocaleLowerCase().includes(searchString) ||
        filter.text?.toString().toLocaleLowerCase().includes(searchString)
      );
    });
  }, [checklist, filters, search]);

  const listHeight = useMemo(() => {
    if (checklist && !searchable) {
      return ITEM_HEIGHT * Math.min(filteredOptions.length, CHECKLIST_ROWS);
    }
    if (filteredOptions.length < 10) return ITEM_HEIGHT * filteredOptions.length;
    return ITEM_HEIGHT * 9;
  }, [checklist, filteredOptions.length, searchable]);

  const handleSearchChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setSearch(e.target.value || '');
    setActiveIndex(0);
  }, []);

  /** Ticks or unticks the option; with `allBut`, ticks every option except it. */
  const toggle = useCallback(
    (value: string, allBut: boolean) => {
      setSelectedMap((prev) => {
        if (multiple) {
          if (allBut && filters) {
            return filters.reduce(
              (acc, filter) => {
                if (filter.value !== value) acc[filter.value as string] = true;
                return acc;
              },
              {} as Record<string, boolean>,
            );
          }
          const newMap = { ...prev };
          if (newMap[value]) delete newMap[value];
          else newMap[value] = true;
          return newMap;
        }
        return prev[value] ? {} : { [value]: true };
      });
    },
    [filters, multiple],
  );

  const handleOptionClick = useCallback(
    (e: React.MouseEvent) => {
      const value = (e.target as HTMLDivElement).getAttribute('data-value');
      if (!value) return;
      // Cmd + click (or Ctrl + click in a tick list) selects every option EXCEPT the clicked one.
      toggle(value, e.metaKey || (!!checklist && e.ctrlKey));
      if (checklist) {
        const index = filteredOptions.findIndex((option) => String(option.value) === value);
        if (index >= 0) setActiveIndex(index);
      }
    },
    [checklist, filteredOptions, toggle],
  );

  /** Ticks (with false, unticks) the options shown, which a search narrows down. */
  const tickShown = useCallback(
    (tick: boolean) =>
      setSelectedMap((prev) => {
        const next = { ...prev };
        filteredOptions.forEach((option) => {
          if (tick) next[option.value as string] = true;
          else delete next[option.value as string];
        });
        return next;
      }),
    [filteredOptions],
  );

  const restoreFocus = useCallback(() => {
    const opener = openerRef.current;
    openerRef.current = null;
    // Once the dropdown has closed.
    setTimeout(() => opener?.focus(), 0);
  }, []);

  const handleReset = useCallback(() => {
    setSelectedMap({});
    if (onReset) onReset();
    if (clearFilters) clearFilters();
  }, [clearFilters, onReset]);

  const handleFilter = useCallback(() => {
    let keys = Object.keys(selectedMap);
    if (checklist) {
      const options = (filters ?? []).map((filter) => String(filter.value));
      keys = options.filter((option) => selectedMap[option]);
      if (keys.length === options.length) keys = [];
    }
    if (onFilter) onFilter(keys);
    confirm();
    if (checklist) restoreFocus();
  }, [checklist, confirm, filters, onFilter, restoreFocus, selectedMap]);

  const OptionRow: React.FC<ListChildComponentProps> = useCallback(
    ({ data, index, style }) => {
      const classes = [css.option];
      const isSelected = !!selectedMap[data[index].value];
      const isJSX = typeof data[index].text !== 'string';
      if (checklist) {
        if (index === activeIndex) classes.push(css.active);
        // The option's title shows a name cut short: its children take no pointer events.
        return (
          <div
            aria-selected={isSelected}
            className={classes.join(' ')}
            data-value={data[index].value}
            id={`${listId}-${index}`}
            role="option"
            style={style}
            title={isJSX ? undefined : data[index].text}
            onClick={handleOptionClick}>
            <span aria-hidden className={css.checkbox}>
              <Checkbox checked={isSelected} tabIndex={-1} />
            </span>
            {isJSX ? data[index].text : <span>{data[index].text}</span>}
          </div>
        );
      }
      if (isSelected) classes.push(css.selected);
      return (
        <div
          className={classes.join(' ')}
          data-value={data[index].value}
          style={style}
          onClick={handleOptionClick}>
          {isJSX ? data[index].text : <span>{data[index].text}</span>}
          <Icon name="checkmark" title="Selected" />
        </div>
      );
    },
    [activeIndex, checklist, handleOptionClick, listId, selectedMap],
  );

  const moveActive = useCallback(
    (index: number) => {
      const next = Math.max(0, Math.min(index, filteredOptions.length - 1));
      setActiveIndex(next);
      virtualListRef.current?.scrollToItem(next);
    },
    [filteredOptions.length],
  );

  /*
   * The tick list's keys. The dropdown would close on the second Tab, so Tab goes round the search
   * box, the list, All, None and OK here; Escape closes without applying.
   */
  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      if (!checklist) return;
      const list = listRef.current;
      const onList = !!list && e.target === list;
      const onSearch = e.target instanceof HTMLInputElement;
      switch (e.key) {
        case 'Tab': {
          e.preventDefault();
          e.stopPropagation();
          const stops: HTMLElement[] = [];
          if (inputRef.current?.input) stops.push(inputRef.current.input);
          if (list) stops.push(list);
          stops.push(...Array.from(footerRef.current?.querySelectorAll('button') ?? []));
          const at = stops.findIndex((stop) => stop.contains(document.activeElement));
          const next = (at + (e.shiftKey ? -1 : 1) + stops.length) % stops.length;
          stops[next]?.focus();
          return;
        }
        case 'Escape':
          e.preventDefault();
          e.stopPropagation();
          close();
          restoreFocus();
          return;
        case 'Enter':
          if (onList || onSearch) {
            e.preventDefault();
            e.stopPropagation();
            handleFilter();
          }
          return;
        case 'ArrowDown':
          if (onSearch) {
            e.preventDefault();
            list?.focus();
          } else if (onList) {
            e.preventDefault();
            moveActive(activeIndex + 1);
          }
          return;
        case 'ArrowUp':
          if (onList) {
            e.preventDefault();
            moveActive(activeIndex - 1);
          }
          return;
        case 'Home':
        case 'End':
          if (onList) {
            e.preventDefault();
            moveActive(e.key === 'Home' ? 0 : filteredOptions.length - 1);
          }
          return;
        case ' ':
          if (onList) {
            e.preventDefault();
            const option = filteredOptions[activeIndex];
            if (option) toggle(String(option.value), e.metaKey || e.ctrlKey);
          }
          return;
      }
    },
    [
      activeIndex,
      checklist,
      close,
      filteredOptions,
      handleFilter,
      moveActive,
      restoreFocus,
      toggle,
    ],
  );

  /*
   * Detect when filter dropdown is being shown and
   * proceed to initialize the selected map of which
   * options are selected.
   */
  useEffect(() => {
    if (prevVisible !== visible && visible) {
      setSearch('');
      setActiveIndex(0);
      if (checklist && document.activeElement instanceof HTMLElement) {
        openerRef.current = document.activeElement;
      }

      const valuesAsList = Array.isArray(values) ? values : [values];
      setSelectedMap(
        valuesAsList.reduce(
          (acc, value) => {
            acc[value] = true;
            return acc;
          },
          {} as Record<string, boolean>,
        ),
      );

      setTimeout(() => {
        if (inputRef.current) inputRef.current.focus({ cursor: 'all' });
        else if (checklist) listRef.current?.focus();
      }, 0);
    }
  }, [checklist, prevVisible, values, visible]);

  const classes = [css.base, themeClass];
  if (checklist) classes.push(css.checklist);
  const list = (
    <FixedSizeList
      height={listHeight}
      itemCount={filteredOptions.length}
      itemData={filteredOptions}
      itemSize={ITEM_HEIGHT}
      ref={virtualListRef}
      width="100%">
      {OptionRow}
    </FixedSizeList>
  );
  return (
    <div
      aria-label={ARIA_LABEL_CONTAINER}
      className={classes.join(' ')}
      style={{ width }}
      onKeyDown={handleKeyDown}>
      {searchable && (
        <div className={css.search}>
          <Input
            allowClear
            aria-label={ARIA_LABEL_INPUT}
            bordered={false}
            placeholder="search filters"
            prefix={<Icon name="search" size="tiny" title="Search" />}
            ref={inputRef}
            value={search}
            onChange={handleSearchChange}
          />
        </div>
      )}
      {checklist ? (
        <div
          aria-activedescendant={
            filteredOptions.length > 0 ? `${listId}-${activeIndex}` : undefined
          }
          aria-label={label}
          aria-multiselectable={multiple}
          className={css.list}
          ref={listRef}
          role="listbox"
          tabIndex={0}>
          {list}
        </div>
      ) : (
        list
      )}
      {checklist ? (
        <div className={css.footer} ref={footerRef}>
          <Button size="small" onClick={() => tickShown(true)}>
            All
          </Button>
          <Button size="small" onClick={() => tickShown(false)}>
            None
          </Button>
          <div className={css.spacer} />
          <Button size="small" type="primary" onClick={handleFilter}>
            OK
          </Button>
        </div>
      ) : (
        <div className={css.footer}>
          <Button
            aria-label={ARIA_LABEL_RESET}
            disabled={Object.keys(selectedMap).length === 0}
            size="small"
            onClick={handleReset}>
            Reset
          </Button>
          <Button aria-label={ARIA_LABEL_APPLY} size="small" type="primary" onClick={handleFilter}>
            OK
          </Button>
        </div>
      )}
    </div>
  );
};

export default TableFilterDropdown;
