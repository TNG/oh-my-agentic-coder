package com.omac.fixture;

public class GreetingService {

    public interface GreetingRepository {
        String fetchGreeting(String name);
    }

    private final GreetingRepository repository;

    public GreetingService(GreetingRepository repository) {
        this.repository = repository;
    }

    public String greet(String name) {
        String template = repository.fetchGreeting(name);
        if (template == null || template.isBlank()) {
            return "Hello, " + name + "!";
        }
        return template;
    }
}