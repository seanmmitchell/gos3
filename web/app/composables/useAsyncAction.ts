/**
 * Wraps an async action with loading/error state and toasts.
 *
 *   const del = useAsyncAction(async (name: string) => s3.deleteBucket(name), {
 *     successMessage: name => `Bucket ${name} deleted`, errorTitle: 'Could not delete bucket'
 *   })
 *   await del.run('photos')   // resolves to the result, or undefined on error
 *
 * Aborted requests never toast.
 */
export interface AsyncActionOptions<A extends unknown[], R> {
  successMessage?: string | ((...args: [...A, R]) => string)
  errorTitle?: string
  /** Show an error toast (default true). */
  toastErrors?: boolean
  /** Rethrow after handling (default false). */
  rethrow?: boolean
}

export function useAsyncAction<A extends unknown[], R>(fn: (...args: A) => Promise<R>, opts: AsyncActionOptions<A, R> = {}) {
  const toast = useToast()
  const loading = ref(false)
  const error = ref<unknown>(null)

  async function run(...args: A): Promise<R | undefined> {
    loading.value = true
    error.value = null
    try {
      const result = await fn(...args)
      if (opts.successMessage) {
        const title = typeof opts.successMessage === 'function' ? opts.successMessage(...args, result) : opts.successMessage
        toast.add({ title, color: 'success', icon: 'i-lucide-circle-check' })
      }
      return result
    } catch (e) {
      error.value = e
      if (opts.toastErrors !== false && !isAbortedError(e)) {
        const d = describeError(e)
        toast.add({
          title: opts.errorTitle ?? d.title,
          description: d.hint ? `${d.message} ${d.hint}` : d.message,
          color: 'error',
          icon: 'i-lucide-circle-alert'
        })
      }
      if (opts.rethrow) throw e
      return undefined
    } finally {
      loading.value = false
    }
  }

  return { run, loading, error }
}

/**
 * Loads data with loading / error state; re-runs when sources change.
 *
 *   const { data, loading, error, refresh } = useLoader(() => s3.listBuckets(), { watch: [connId] })
 */
export function useLoader<T>(fetcher: () => Promise<T>, opts: { immediate?: boolean, watch?: import('vue').WatchSource[], initial?: T } = {}) {
  const data = ref<T | undefined>(opts.initial) as import('vue').Ref<T | undefined>
  const loading = ref(false)
  const error = ref<unknown>(null)
  let seq = 0

  async function refresh(): Promise<T | undefined> {
    const my = ++seq
    loading.value = true
    error.value = null
    try {
      const r = await fetcher()
      if (my === seq) data.value = r
      return r
    } catch (e) {
      if (my === seq) error.value = e
      return undefined
    } finally {
      if (my === seq) loading.value = false
    }
  }

  if (opts.watch?.length) watch(opts.watch, () => void refresh())
  if (opts.immediate !== false) void refresh()

  return { data, loading, error, refresh }
}
