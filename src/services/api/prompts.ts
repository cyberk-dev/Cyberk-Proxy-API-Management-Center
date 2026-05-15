import { apiClient } from './client';
import type { PromptDetail, PromptUsersResponse } from '@/types/prompts';

const PROMPTS_TIMEOUT_MS = 30 * 1000;

export const promptsApi = {
  listUsers: () =>
    apiClient.get<PromptUsersResponse>('/v0/management/prompts/users', {
      timeout: PROMPTS_TIMEOUT_MS,
    }),

  getDetail: (keyOrHash: string, limit?: number) =>
    apiClient.get<PromptDetail>(`/v0/management/prompts/users/${encodeURIComponent(keyOrHash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
      params: limit ? { limit } : undefined,
    }),
};
