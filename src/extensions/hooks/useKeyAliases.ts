import { useCallback, useEffect, useRef, useState } from 'react';
import { useConfigStore } from '@/stores';
import {
  type AliasMap,
  readAliases,
  writeAlias as writeAliasApi
} from '../services/aliasStore';

interface UseKeyAliasesState {
  aliases: AliasMap;
  loading: boolean;
  error: string | null;
  reload: () => Promise<void>;
  saveAlias: (apiKey: string, alias: string) => Promise<void>;
}

export function useKeyAliases(): UseKeyAliasesState {
  const [aliases, setAliases] = useState<AliasMap>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await readAliases();
      if (mountedRef.current) setAliases(next);
    } catch (e) {
      if (mountedRef.current) {
        setError(e instanceof Error ? e.message : 'Failed to load aliases');
      }
    } finally {
      if (mountedRef.current) setLoading(false);
    }
  }, []);

  const saveAlias = useCallback(async (apiKey: string, alias: string) => {
    const next = await writeAliasApi(apiKey, alias);
    if (mountedRef.current) {
      setAliases(next);
      // Sync the global config cache so sidebar / ConfigPage re-read the latest
      // YAML (which now contains our new ui-aliases:) on next refresh.
      try {
        useConfigStore.getState().clearCache();
      } catch {
        /* non-fatal */
      }
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  return { aliases, loading, error, reload, saveAlias };
}
