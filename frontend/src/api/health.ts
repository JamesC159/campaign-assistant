import { useQuery } from '@tanstack/react-query'
import { fetchHealth } from './client'

export function useHealth() {
  return useQuery({
    queryKey: ['health'],
    queryFn: ({ signal }) => fetchHealth(signal),
    refetchInterval: 15_000,
    retry: false,
  })
}
