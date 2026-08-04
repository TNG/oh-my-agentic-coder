package com.omac.fixture;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import com.omac.fixture.GreetingService.GreetingRepository;
import org.junit.jupiter.api.Test;

class GreetingServiceTest {

    @Test
    void greetFallsBackToDefaultWhenRepositoryReturnsBlank() {
        GreetingRepository repo = mock(GreetingRepository.class);
        when(repo.fetchGreeting("World")).thenReturn("   ");
        GreetingService service = new GreetingService(repo);
        assertEquals("Hello, World!", service.greet("World"));
    }

    @Test
    void greetUsesRepositoryTemplate() {
        GreetingRepository repo = mock(GreetingRepository.class);
        when(repo.fetchGreeting("Alice")).thenReturn("Hi there, Alice!");
        GreetingService service = new GreetingService(repo);
        assertEquals("Hi there, Alice!", service.greet("Alice"));
    }
}