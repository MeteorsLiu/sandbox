#include <stdio.h>
#include <sys/reboot.h>
#include <termios.h>
#include <unistd.h>

int main(void) {
    sync();
    tcdrain(STDOUT_FILENO);
    reboot(RB_AUTOBOOT);
    perror("reboot");
    return 1;
}
