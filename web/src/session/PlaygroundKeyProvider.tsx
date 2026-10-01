/**
 * The Playground API key, held in React state only.
 *
 * Explicitly *not* persisted: no localStorage, no sessionStorage, no cookie, no
 * URL, no console output. The provider is mounted with `key={orgId}` inside the
 * authenticated layout, so switching organizations or logging out unmounts it
 * and the key is gone; unmounting the provider (leaving the app shell) does the
 * same. A page reload starts with no key, which is the intended behaviour.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';

export type PlaygroundKeySource = 'created' | 'manual';

export interface PlaygroundKeyValue {
  apiKey: string | null;
  source: PlaygroundKeySource | null;
  /** When the key was placed in memory (used for the "memory only" notice). */
  setKey: (apiKey: string, source: PlaygroundKeySource) => void;
  clearKey: () => void;
}

const PlaygroundKeyContext = createContext<PlaygroundKeyValue | null>(null);

export function PlaygroundKeyProvider({
  orgId,
  children,
}: {
  orgId: string;
  children: ReactNode;
}): ReactNode {
  const [state, setState] = useState<{ apiKey: string | null; source: PlaygroundKeySource | null }>(
    { apiKey: null, source: null },
  );

  // Defence in depth: even if this provider is not remounted, changing the
  // organization drops the previous organization's key.
  useEffect(() => {
    setState({ apiKey: null, source: null });
  }, [orgId]);

  const setKey = useCallback((apiKey: string, source: PlaygroundKeySource) => {
    setState({ apiKey, source });
  }, []);

  const clearKey = useCallback(() => {
    setState({ apiKey: null, source: null });
  }, []);

  const value = useMemo<PlaygroundKeyValue>(
    () => ({ apiKey: state.apiKey, source: state.source, setKey, clearKey }),
    [clearKey, setKey, state.apiKey, state.source],
  );

  return <PlaygroundKeyContext.Provider value={value}>{children}</PlaygroundKeyContext.Provider>;
}

export function usePlaygroundKey(): PlaygroundKeyValue {
  const value = useContext(PlaygroundKeyContext);
  if (!value) {
    throw new Error('usePlaygroundKey must be used inside <PlaygroundKeyProvider>');
  }
  return value;
}
