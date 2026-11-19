import { logout } from "./api";

// One sign-out path for both routes: revoke the session on the server, then re-read it so the
// application shows the sign-in route.
export async function signOut(refresh: () => Promise<void>): Promise<void> {
  try {
    await logout();
  } finally {
    await refresh();
  }
}
