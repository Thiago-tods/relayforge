// The subscription pages use the same stubbed-fetch harness as the destination pages: it answers the session and CSRF
// routes, serves "METHOD /pathname" routes (the query string is not part of the key) and answers 404 for the rest.
export { mountDestinations as mountSubscriptions, requests } from './destinationHarness'
export type { HarnessSetup } from './destinationHarness'
